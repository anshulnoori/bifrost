// Package codex serves ChatGPT subscription inference through owner-bound OAuth.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/providers/openai"
	utils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/valyala/fasthttp"
)

const baseURL = "https://chatgpt.com/backend-api/codex"

// Unimplemented operations inherit explicit unsupported-operation errors from
// the restricted OpenAI provider. Only methods below can resolve a credential.
type Provider struct {
	*openai.OpenAIProvider
	resolve      func(*schemas.BifrostContext, schemas.Key) (string, string, error)
	streamClient *fasthttp.Client
	logger       schemas.Logger
	idle         int
	url          string
}

func New(config *schemas.ProviderConfig, logger schemas.Logger) (*Provider, error) {
	if config.CustomProviderConfig != nil || config.NetworkConfig.BaseURL != "" || len(config.NetworkConfig.ExtraHeaders) != 0 {
		return nil, errors.New("codex does not allow custom base URLs, provider overrides, or extra headers")
	}
	c := *config
	c.CheckAndSetDefaults()
	c.NetworkConfig.BaseURL = baseURL
	c.CustomProviderConfig = &schemas.CustomProviderConfig{CustomProviderKey: string(schemas.Codex), BaseProviderType: schemas.OpenAI,
		AllowedRequests: &schemas.AllowedRequests{Passthrough: true, PassthroughStream: true}}
	c.SendBackRawRequest, c.SendBackRawResponse = false, false
	client := &fasthttp.Client{MaxConnsPerHost: c.NetworkConfig.MaxConnsPerHost, MaxIdleConnDuration: 30 * time.Second}
	client = utils.ConfigureProxy(client, c.ProxyConfig, logger)
	client = utils.ConfigureDialer(client, c.NetworkConfig.AllowPrivateNetwork)
	client = utils.ConfigureTLS(client, c.NetworkConfig, logger)
	return &Provider{OpenAIProvider: openai.NewOpenAIProvider(&c, logger), resolve: c.CodexCredential,
		streamClient: utils.BuildStreamingClient(client), logger: logger, idle: c.NetworkConfig.StreamIdleTimeoutInSeconds, url: baseURL}, nil
}

func failure(message string) *schemas.BifrostError {
	return &schemas.BifrostError{IsBifrostError: true, StatusCode: schemas.Ptr(400), AllowFallbacks: schemas.Ptr(false),
		Error: &schemas.ErrorField{Message: message}, ExtraFields: schemas.BifrostErrorExtraFields{Provider: schemas.Codex}}
}

func (p *Provider) auth(ctx *schemas.BifrostContext, key schemas.Key, model string) (map[string]string, *schemas.BifrostError) {
	if p.resolve == nil {
		return nil, failure("codex credential resolver is not configured")
	}
	if ctx == nil || ctx.Grant() == nil {
		return nil, failure("codex requires gateway admission")
	}
	if access := ctx.Grant().Access(); access != nil {
		if !access.IsModelAllowed(string(schemas.Codex), model) {
			return nil, failure("codex provider is not allowed")
		}
		if ids, restricted := access.KeysForModel(string(schemas.Codex), model); restricted && !slices.Contains(ids, key.ID) {
			return nil, failure("codex account is not allowed")
		}
	} else if ctx.Grant().Identity() == nil || ctx.Grant().Identity().Presented() || ctx.Grant().Limits() == nil {
		// A settled anonymous grant is intentional only when the operator has
		// disabled inference authentication. A presented but unresolved key is not.
		return nil, failure("codex requires gateway admission")
	}
	access, account, err := p.resolve(ctx, key)
	if err != nil {
		if errors.Is(err, schemas.ErrCodexReserve) {
			return nil, failure(schemas.ErrCodexReserve.Error())
		}
		return nil, failure("codex credential unavailable; check connection status or reconnect")
	}
	if access == "" || account == "" || strings.ContainsAny(access+account, "\r\n") {
		return nil, failure("invalid codex credential")
	}
	return map[string]string{"Authorization": "Bearer " + access, "ChatGPT-Account-ID": account, "OpenAI-Beta": "responses=v1", "originator": "bifrost"}, nil
}

// prepare preserves unknown wire fields and ordering. Explicitly unsupported
// options fail rather than silently degrading the client's request.
func prepare(body []byte, model string) ([]byte, error) {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, errors.New("codex requires a JSON object")
	}
	seen := make(map[string]bool)
	duplicate := false
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
		name := key.String()
		if seen[name] {
			duplicate = true
			return false
		}
		seen[name] = true
		return true
	})
	if duplicate {
		return nil, errors.New("codex rejects duplicate request fields")
	}
	for _, field := range []string{"max_output_tokens", "temperature", "top_p", "previous_response_id"} {
		if gjson.GetBytes(body, field).Exists() {
			return nil, fmt.Errorf("codex does not support %s", field)
		}
	}
	for _, field := range []string{"store", "background"} {
		v := gjson.GetBytes(body, field)
		if v.Exists() && v.Raw != "false" && v.Raw != "null" {
			return nil, fmt.Errorf("codex requires %s=false", field)
		}
	}
	if model == "" || gjson.GetBytes(body, "model").String() != model {
		return nil, errors.New("codex body model differs from admitted model")
	}
	var err error
	body, err = sjson.SetBytes(body, "store", false)
	if err != nil {
		return nil, err
	}
	body, err = sjson.SetBytes(body, "stream", true)
	if err != nil {
		return nil, err
	}
	if !gjson.GetBytes(body, "instructions").Exists() {
		body, err = sjson.SetBytes(body, "instructions", "")
	}
	return body, err
}

func (p *Provider) ResponsesStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, finalize func(context.Context), key schemas.Key, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	headers, authErr := p.auth(ctx, key, request.Model)
	if authErr != nil {
		return nil, authErr
	}
	if utils.IsLargePayloadPassthroughEnabled(ctx) {
		return nil, failure("codex does not support large-payload bypass mode")
	}
	r := *request
	params := schemas.ResponsesParameters{}
	if r.Params != nil {
		params = *r.Params
	}
	r.Params = &params
	// Validate before OpenAI's converter can normalize away unsupported knobs.
	if params.Temperature != nil || params.TopP != nil || params.MaxOutputTokens != nil || params.PreviousResponseID != nil {
		return nil, failure("codex does not support temperature, top_p, max_output_tokens, or previous_response_id")
	}
	if params.Store != nil && *params.Store {
		return nil, failure("codex requires store=false")
	}
	params.Store = schemas.Ptr(false)
	if params.Instructions == nil {
		params.Instructions = schemas.Ptr("")
	}
	// Validate both the typed request and any caller-selected raw body. The shared
	// OpenAI streaming machinery then retains cancellation, hooks and accounting.
	if raw, ok := utils.CheckAndGetRawRequestBody(ctx, &r); ok {
		body, err := prepare(raw, r.Model)
		if err != nil {
			return nil, failure(err.Error())
		}
		r.RawRequestBody = body
	} else {
		body, bodyErr := utils.CheckContextAndGetRequestBody(ctx, &r, func() (utils.RequestBodyWithExtraParams, error) {
			return openai.ToOpenAIResponsesRequest(ctx, &r), nil
		})
		if bodyErr != nil {
			return nil, bodyErr
		}
		if _, err := prepare(body, r.Model); err != nil {
			return nil, failure(err.Error())
		}
	}
	return openai.HandleOpenAIResponsesStreaming(ctx, p.streamClient, p.url+"/responses", &r, headers, nil, p.idle,
		false, false, schemas.Codex, hook, nil, nil, nil, nil, nil, p.logger, finalize)
}

func (p *Provider) Responses(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	ch, err := p.ResponsesStream(ctx, func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return r, e
	}, func(context.Context) {}, key, request)
	if err != nil {
		return nil, err
	}
	var result *schemas.BifrostResponsesResponse
	var streamErr *schemas.BifrostError
	items := make(map[int]schemas.ResponsesMessage)
	for chunk := range ch {
		if chunk.BifrostError != nil {
			streamErr = chunk.BifrostError
		}
		if r := chunk.BifrostResponsesStreamResponse; r != nil && r.Type == schemas.ResponsesStreamResponseTypeOutputItemDone && r.Item != nil && r.OutputIndex != nil {
			items[*r.OutputIndex] = *r.Item
		}
		if r := chunk.BifrostResponsesStreamResponse; r != nil && (r.Type == schemas.ResponsesStreamResponseTypeCompleted || r.Type == schemas.ResponsesStreamResponseTypeIncomplete) {
			result = r.Response
			if result != nil {
				result.ExtraFields = r.ExtraFields
			}
		}
	}
	if streamErr != nil {
		return nil, streamErr
	}
	if result == nil {
		return nil, failure("codex stream ended without a terminal response")
	}
	// Codex can omit output from the terminal event after emitting completed
	// items. Reconstruct by output index, not event arrival order.
	if len(result.Output) == 0 {
		for _, index := range slices.Sorted(maps.Keys(items)) {
			result.Output = append(result.Output, items[index])
		}
	}
	result.ExtraFields.RequestType = schemas.ResponsesRequest
	return result, nil
}

func (p *Provider) ChatCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	if err := validateChat(ctx, request); err != nil {
		return nil, err
	}
	response, err := p.Responses(ctx, key, request.ToResponsesRequest())
	if err != nil {
		return nil, err
	}
	return response.ToBifrostChatResponse(), nil
}

func (p *Provider) ChatCompletionStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, finalize func(context.Context), key schemas.Key, request *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	if err := validateChat(ctx, request); err != nil {
		return nil, err
	}
	// Responses output indexes include reasoning and messages. Chat tool indexes
	// are a separate dense sequence; subtracting one collapses tools at 0 and 1.
	toolIndexes := map[int]uint16{}
	var responseID string
	var created int
	convert := func(ctx *schemas.BifrostContext, r *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		if r != nil && r.ResponsesStreamResponse != nil {
			event := r.ResponsesStreamResponse
			r.ChatResponse = event.ToBifrostChatResponse()
			if r.ChatResponse.ID != "" {
				responseID = r.ChatResponse.ID
				created = r.ChatResponse.Created
			}
			r.ChatResponse.ID, r.ChatResponse.Created, r.ChatResponse.Model = responseID, created, request.Model
			for i := range r.ChatResponse.Choices {
				choice := &r.ChatResponse.Choices[i]
				if choice.ChatStreamResponseChoice == nil || choice.Delta == nil || len(choice.Delta.ToolCalls) == 0 {
					continue
				}
				if event.OutputIndex == nil {
					return nil, failure("codex tool event has no output index")
				}
				index, ok := toolIndexes[*event.OutputIndex]
				if !ok {
					index = uint16(len(toolIndexes))
					toolIndexes[*event.OutputIndex] = index
				}
				for j := range choice.Delta.ToolCalls {
					choice.Delta.ToolCalls[j].Index = index
				}
			}
			r.ResponsesStreamResponse = nil
		}
		return hook(ctx, r, err)
	}
	return p.ResponsesStream(ctx, convert, finalize, key, request.ToResponsesRequest())
}

func validateChat(ctx *schemas.BifrostContext, request *schemas.BifrostChatRequest) *schemas.BifrostError {
	if _, raw := utils.CheckAndGetRawRequestBody(ctx, request); raw {
		return failure("codex Chat does not support raw-body mode; use Responses passthrough")
	}
	if request.Params == nil {
		return nil
	}
	body, err := schemas.MarshalSorted(request.Params)
	if err != nil {
		return failure("invalid codex Chat parameters")
	}
	var unsupported string
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
		switch key.String() {
		case "parallel_tool_calls", "prompt_cache_key", "prompt_cache_retention", "prompt_cache_options", "safety_identifier", "service_tier", "store", "temperature", "top_logprobs", "top_p", "max_completion_tokens", "metadata", "stream_options", "tools", "tool_choice", "reasoning", "response_format", "verbosity":
			return true
		default:
			unsupported = key.String()
			return false
		}
	})
	if unsupported != "" {
		return failure("codex Chat does not support " + unsupported)
	}
	return nil
}

func (p *Provider) passthrough(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughRequest, *schemas.BifrostError) {
	if request.Method != http.MethodPost || (request.Path != "/responses" && request.Path != "/v1/responses" && request.Path != "/backend-api/codex/responses") || request.RawQuery != "" {
		return nil, failure("codex passthrough supports only POST /responses")
	}
	body, err := prepare(request.Body, request.Model)
	if err != nil {
		return nil, failure(err.Error())
	}
	headers, authErr := p.auth(ctx, key, request.Model)
	if authErr != nil {
		return nil, authErr
	}
	headers["Content-Type"] = "application/json"
	headers["Accept"] = "text/event-stream"
	r := *request
	r.Body, r.SafeHeaders, r.UpstreamURL, r.Path = body, headers, p.url, "/responses"
	return &r, nil
}

func (p *Provider) PassthroughStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, finalize func(context.Context), key schemas.Key, request *schemas.BifrostPassthroughRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	r, err := p.passthrough(ctx, key, request)
	if err != nil {
		return nil, err
	}
	return p.OpenAIProvider.PassthroughStream(ctx, hook, finalize, schemas.Key{}, r)
}

func (p *Provider) Passthrough(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	ch, err := p.PassthroughStream(ctx, func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return r, e
	}, func(context.Context) {}, key, request)
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	var result schemas.BifrostPassthroughResponse
	var streamErr *schemas.BifrostError
	for chunk := range ch {
		if chunk.BifrostError != nil {
			streamErr = chunk.BifrostError
		}
		if r := chunk.BifrostPassthroughResponse; r != nil {
			result = *r
			if body.Len()+len(r.Body) > 32<<20 {
				streamErr = failure("codex unary response exceeds 32 MiB; use streaming")
			}
			if streamErr == nil {
				body.Write(r.Body)
			}
		}
	}
	if streamErr != nil {
		return nil, streamErr
	}
	if result.StatusCode != http.StatusOK {
		result.Body = body.Bytes()
		return &result, nil
	}
	reader := utils.GetSSEDataReader(ctx, bytes.NewReader(body.Bytes()))
	items := make(map[int]json.RawMessage)
	for {
		data, readErr := reader.ReadDataLine()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, failure("invalid codex event stream")
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.output_item.done":
			index, item := gjson.GetBytes(data, "output_index"), gjson.GetBytes(data, "item")
			if index.Exists() && item.IsObject() {
				items[int(index.Int())] = json.RawMessage(item.Raw)
			}
		case "response.failed", "error":
			return nil, failure("codex response failed")
		case "response.completed", "response.incomplete":
			response := gjson.GetBytes(data, "response")
			if !response.IsObject() {
				return nil, failure("invalid codex terminal response")
			}
			result.Body = []byte(response.Raw)
			if len(response.Get("output").Array()) == 0 && len(items) > 0 {
				output := make([]json.RawMessage, 0, len(items))
				for _, index := range slices.Sorted(maps.Keys(items)) {
					output = append(output, items[index])
				}
				result.Body, _ = sjson.SetBytes(result.Body, "output", output)
			}
			result.Headers = map[string]string{"Content-Type": "application/json"}
			result.ExtraFields.RequestType = schemas.PassthroughRequest
			return &result, nil
		}
	}
	return nil, failure("codex stream ended without a terminal response")
}

// Catalog results are account-scoped and filtered against gateway admission.
// Never cache one subscriber's catalog globally or invent a static model list.
func (p *Provider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	result := &schemas.BifrostListModelsResponse{Data: []schemas.Model{}, ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Codex}}
	seen := make(map[string]bool)
	var lastError *schemas.BifrostError
	for _, key := range keys {
		models, err := p.listKeyModels(ctx, key)
		if err != nil {
			lastError = err
			result.KeyStatuses = append(result.KeyStatuses, schemas.KeyStatus{KeyID: key.ID, Provider: schemas.Codex, Status: schemas.KeyStatusListModelsFailed, Error: err})
			continue
		}
		result.KeyStatuses = append(result.KeyStatuses, schemas.KeyStatus{KeyID: key.ID, Provider: schemas.Codex, Status: schemas.KeyStatusSuccess})
		for _, model := range models {
			if !seen[model.ID] {
				seen[model.ID] = true
				result.Data = append(result.Data, model)
			}
		}
	}
	if len(result.Data) == 0 && lastError != nil {
		lastError.ExtraFields.KeyStatuses = result.KeyStatuses
		return nil, lastError
	}
	return result.ApplyPagination(request.PageSize, request.PageToken), nil
}

func (p *Provider) listKeyModels(ctx *schemas.BifrostContext, key schemas.Key) ([]schemas.Model, *schemas.BifrostError) {
	headers, err := p.auth(ctx, key, "")
	if err != nil {
		return nil, err
	}
	raw, err := p.OpenAIProvider.Passthrough(ctx, schemas.Key{}, &schemas.BifrostPassthroughRequest{
		Method: http.MethodGet, Path: "/models", RawQuery: "client_version=0.0.0", UpstreamURL: p.url, SafeHeaders: headers,
	})
	if err != nil {
		return nil, err
	}
	if raw.StatusCode != 200 {
		return nil, failure(fmt.Sprintf("codex catalog returned HTTP %d", raw.StatusCode))
	}
	models := gjson.GetBytes(raw.Body, "models")
	if !models.IsArray() {
		return nil, failure("invalid codex model catalog")
	}
	result := []schemas.Model{}
	for _, m := range models.Array() {
		id := m.Get("slug").String()
		if id == "" || (ctx.Grant().Access() != nil && !ctx.Grant().Access().IsModelAllowed("codex", id)) || !key.Models.IsAllowed(id) || key.BlacklistedModels.IsBlocked(id) {
			continue
		}
		model := schemas.Model{ID: "codex/" + id, Name: schemas.Ptr(m.Get("display_name").String()), OwnedBy: schemas.Ptr("openai")}
		if n := m.Get("context_window").Int(); n > 0 {
			model.ContextLength = schemas.Ptr(int(n))
		}
		result = append(result, model)
	}
	return result, nil
}
