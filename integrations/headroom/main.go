package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

type contextKey string

const eventKey contextKey = "headroom-event"
const threadKey contextKey = "headroom-thread"

var current atomic.Pointer[bridge]

func GetName() string { return "headroom" }
func Init(raw any) error {
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var config Config
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&config); err != nil {
		return err
	}
	b, err := newBridge(config)
	if err != nil {
		return err
	}
	if err = configureMonitor(config); err != nil {
		b.close()
		return err
	}
	old := current.Swap(b)
	if old != nil {
		old.close()
	}
	return nil
}
func Cleanup() error {
	// Go's loader shares package state between DynamicPlugin wrappers. Bifrost
	// calls the OLD wrapper's Cleanup after the replacement's Init (and for
	// multiple interfaces). It must not clear the newly published bridge or
	// close its monitor. The single bounded listener therefore lives until
	// process exit; Init closes the replaced client's idle connections.
	if b := current.Load(); b != nil && b.client != nil {
		b.client.CloseIdleConnections()
	}
	return nil
}
func main() {}

// This hook captures an untrusted partition label, never an identity. Its value
// cannot authorize compression or override the governance-derived project.
func HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	for name, thread := range req.Headers {
		if strings.EqualFold(name, "x-headroom-thread") && len(thread) <= 128 {
			ctx.SetValue(threadKey, thread)
			break
		}
	}
	return nil, nil
}
func HTTPTransportPostHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, _ *schemas.HTTPResponse) error {
	return nil
}
func HTTPTransportStreamChunkHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	return chunk, nil
}
func PreRequestHook(_ *schemas.BifrostContext, _ *schemas.BifrostRequest) error { return nil }

func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	b := current.Load()
	if b == nil {
		return req, nil, nil
	}
	return b.pre(ctx, req)
}

func (b *bridge) pre(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	provider, model, _ := req.GetRequestFields()
	event := &Event{Started: time.Now(), Provider: string(provider), Model: model, Status: "bypassed", Quality: "not_evaluated"}
	ctx.SetValue(eventKey, event)
	bypass := func(reason string) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
		event.Reason = reason
		return req, nil, nil
	}
	if !b.config.Enabled {
		return bypass("disabled")
	}
	grant := ctx.Grant()
	if grant == nil || grant.Identity() == nil || grant.Limits() == nil {
		return bypass("unresolved_governance")
	}
	id := grant.Identity()
	// Bifrost intentionally leaves Access nil for a request with no presented
	// credential. Limits is still settled by admission. Only the operator's
	// explicit gateway-wide scope admits that case; unresolved credentials never do.
	if grant.Access() == nil && (b.config.Scope != "gateway" || id.Presented()) {
		return bypass("unresolved_governance")
	}
	project := ""
	if id.Project() != nil {
		project = id.Project().ID
	}
	principal := ""
	if id.VirtualKey() != nil {
		principal = "vk:" + id.VirtualKey().ID
	}
	if principal == "" && id.User() != nil {
		principal = "user:" + id.User().ID
	}
	if principal == "" && b.config.Scope == "gateway" {
		principal = "gateway"
	}
	if principal == "" {
		return bypass("missing_principal")
	}
	if b.config.VirtualKeyID != "" {
		if id.VirtualKey() == nil || id.VirtualKey().ID != b.config.VirtualKeyID {
			return bypass("virtual_key_not_enabled")
		}
		// A configured project remains an additional restriction when present.
		if b.config.ProjectID != "" && project != b.config.ProjectID {
			return bypass("project_not_enabled")
		}
	} else if (b.config.ProjectID == "" && b.config.Scope != "gateway") || (b.config.ProjectID != "" && project != b.config.ProjectID) {
		return bypass("project_not_enabled")
	}
	session, _ := ctx.Value(schemas.BifrostContextKeySessionID).(string)
	if session == "" {
		// Marker-free compression has no continuation state. A single request
		// is its own partition when the client does not supply a session.
		session, _ = ctx.Value(schemas.BifrostContextKeyRequestID).(string)
	}
	thread, _ := ctx.Value(threadKey).(string)
	if thread == "" {
		thread = session
	}
	if session == "" || len(session) > 256 || thread == "" {
		return bypass("missing_session_or_thread")
	}
	event.Project = project
	event.Principal = b.scopeID("principal", event.Project, principal)
	event.Thread = b.scopeID("thread", event.Project, principal, session, thread)
	scope := b.scopeID(event.Project, principal, session, thread, string(provider), model)
	// Copies are committed only after validation. Fallbacks never inherit a
	// partially patched request or mutations made to the caller's input slices.
	body, protocol, commit, reason := requestBody(ctx, req)
	if reason != "" {
		return bypass(reason)
	}
	if int64(len(body)) > b.config.MaxBodyBytes {
		return bypass("body_limit")
	}
	paths, texts, reason := slots(body, protocol, b.config.MinTextBytes)
	if reason != "" {
		return bypass(reason)
	}
	event.Eligible = true
	start := time.Now()
	out, counts, err := b.compress(ctx, model, scope, texts)
	event.CompressionMS = float64(time.Since(start).Microseconds()) / 1000
	if event.BudgetAlert {
		ctx.Log(schemas.LogLevelWarn, "headroom monthly cost reservations reached the proposed $10 alert threshold")
	}
	if errors.Is(err, errCostGate) {
		return bypass("budget_or_concurrency")
	}
	if err == nil {
		var patched []byte
		patched, err = patchSlots(body, paths, out)
		if err == nil && !bytes.Equal(patched, body) {
			req, err = commit(patched)
			event.Status = "compressed"
		}
		if err == nil {
			event.Estimate = &counts
			if event.Status != "compressed" {
				event.Reason = "unchanged"
			}
			return req, nil, nil
		}
	}
	event.Status, event.Reason = "failed", "sidecar_error"
	if b.config.FailurePolicy == "closed" || ctx.Err() != nil {
		return req, &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{IsBifrostError: true, StatusCode: schemas.Ptr(503), AllowFallbacks: schemas.Ptr(false), Error: &schemas.ErrorField{Message: "headroom compression failed"}}}, nil
	}
	return req, nil, nil
}

type commitBody func([]byte) (*schemas.BifrostRequest, error)

func requestBody(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) ([]byte, string, commitBody, string) {
	if req.PassthroughRequest != nil {
		p := req.PassthroughRequest
		protocol := ""
		if p.Method != "POST" || p.RawQuery != "" {
			return nil, "", nil, "unsupported_endpoint"
		}
		switch p.Path {
		case "/v1/chat/completions":
			protocol = "chat"
		case "/v1/responses", "/responses":
			protocol = "responses"
		case "/v1/messages":
			protocol = "anthropic"
		default:
			return nil, "", nil, "unsupported_endpoint"
		}
		if gjson.GetBytes(p.Body, "model").Str != p.Model {
			return nil, "", nil, "model_mismatch"
		}
		// Compression changes only selected input text. Streaming flags, headers,
		// response chunks and provider finalization remain owned by the provider.
		return p.Body, protocol, func(body []byte) (*schemas.BifrostRequest, error) {
			r := *req
			copy := *p
			copy.Body = body
			r.PassthroughRequest = &copy
			return &r, nil
		}, ""
	}
	if raw, _ := ctx.Value(schemas.BifrostContextKeyUseRawRequestBody).(bool); raw {
		return nil, "", nil, "raw_override"
	}
	if req.ChatRequest != nil && (req.RequestType == schemas.ChatCompletionRequest || req.RequestType == schemas.ChatCompletionStreamRequest) {
		body, err := schemas.MarshalSorted(struct {
			Messages []schemas.ChatMessage   `json:"messages"`
			Params   *schemas.ChatParameters `json:"params,omitempty"`
		}{req.ChatRequest.Input, req.ChatRequest.Params})
		if err != nil {
			return nil, "", nil, "invalid_request"
		}
		return body, "chat", func(body []byte) (*schemas.BifrostRequest, error) {
			r := *req
			copy := *req.ChatRequest
			copy.Input = append([]schemas.ChatMessage(nil), copy.Input...)
			for i, msg := range copy.Input {
				if msg.Role == schemas.ChatMessageRoleTool && msg.Content != nil && msg.Content.ContentStr != nil {
					text := gjson.GetBytes(body, "messages."+strconv.Itoa(i)+".content").Str
					content := *msg.Content
					content.ContentStr = &text
					copy.Input[i].Content = &content
				}
			}
			r.ChatRequest = &copy
			return &r, nil
		}, ""
	}
	if req.ResponsesRequest != nil && (req.RequestType == schemas.ResponsesRequest || req.RequestType == schemas.ResponsesStreamRequest) {
		body, err := schemas.MarshalSorted(struct {
			Input  []schemas.ResponsesMessage   `json:"input"`
			Params *schemas.ResponsesParameters `json:"params,omitempty"`
		}{req.ResponsesRequest.Input, req.ResponsesRequest.Params})
		if err != nil {
			return nil, "", nil, "invalid_request"
		}
		return body, "responses", func(body []byte) (*schemas.BifrostRequest, error) {
			r := *req
			copy := *req.ResponsesRequest
			copy.Input = append([]schemas.ResponsesMessage(nil), copy.Input...)
			for i, msg := range copy.Input {
				if msg.Type != nil && (*msg.Type == "function_call_output" || *msg.Type == "custom_tool_call_output") && msg.ResponsesToolMessage != nil && msg.Output != nil && msg.Output.ResponsesToolCallOutputStr != nil {
					text := gjson.GetBytes(body, "input."+strconv.Itoa(i)+".output").Str
					tool := *msg.ResponsesToolMessage
					output := *msg.Output
					output.ResponsesToolCallOutputStr = &text
					tool.Output = &output
					copy.Input[i].ResponsesToolMessage = &tool
				}
			}
			r.ResponsesRequest = &copy
			return &r, nil
		}, ""
	}
	return nil, "", nil, "unsupported_endpoint"
}

func PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	event, _ := ctx.Value(eventKey).(*Event)
	if event == nil {
		return resp, bifrostErr, nil
	}
	// Serialize once at terminal post-hook; never accumulate chunks in context.
	if stream, _ := ctx.Value(schemas.BifrostContextKeyStreamEndIndicator).(bool); bifrostErr == nil && strings.Contains(string(requestType(resp, bifrostErr)), "stream") && !stream {
		return resp, bifrostErr, nil
	}
	if event.Done.CompareAndSwap(false, true) {
		event.TaskMS = float64(time.Since(event.Started).Microseconds()) / 1000
		if resp != nil {
			event.Usage = providerUsage(resp)
		}
		event.NetworkRetries, _ = ctx.Value(schemas.BifrostContextKeyNumberOfRetries).(int)
		if bifrostErr != nil {
			event.ProviderFailed = true
		}
		ledger.add(event)
		ctx.Log(schemas.LogLevelInfo, fmt.Sprintf("headroom status=%s reason=%s quality=not_evaluated compression_ms=%.3f", event.Status, event.Reason, event.CompressionMS))
	}
	return resp, bifrostErr, nil
}
func requestType(resp *schemas.BifrostResponse, err *schemas.BifrostError) schemas.RequestType {
	if resp != nil && resp.GetExtraFields() != nil {
		return resp.GetExtraFields().RequestType
	}
	if err != nil {
		return err.ExtraFields.RequestType
	}
	return ""
}
