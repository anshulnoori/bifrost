// Package chatgpt provides ChatGPT-plan inference through Sign in with ChatGPT.
package chatgpt

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

const baseURL = "https://api.openai.com/v1"

type Provider struct {
	*openai.OpenAIProvider
	resolve func(*schemas.BifrostContext, schemas.Key) (string, error)
	client  *fasthttp.Client
	logger  schemas.Logger
	idle    int
	url     string
}

func New(config *schemas.ProviderConfig, logger schemas.Logger) (*Provider, error) {
	if config.CustomProviderConfig != nil || config.NetworkConfig.BaseURL != "" || len(config.NetworkConfig.ExtraHeaders) != 0 {
		return nil, errors.New("ChatGPT does not allow custom base URLs, provider overrides, or extra headers")
	}
	c := *config
	c.CheckAndSetDefaults()
	c.NetworkConfig.BaseURL = baseURL
	c.CustomProviderConfig = &schemas.CustomProviderConfig{CustomProviderKey: string(schemas.ChatGPT), BaseProviderType: schemas.OpenAI,
		AllowedRequests: &schemas.AllowedRequests{Passthrough: true, PassthroughStream: true}}
	c.SendBackRawRequest, c.SendBackRawResponse = false, false
	client := &fasthttp.Client{MaxConnsPerHost: c.NetworkConfig.MaxConnsPerHost, MaxIdleConnDuration: 30 * time.Second}
	client = utils.ConfigureProxy(client, c.ProxyConfig, logger)
	client = utils.ConfigureDialer(client, c.NetworkConfig.AllowPrivateNetwork)
	client = utils.ConfigureTLS(client, c.NetworkConfig, logger)
	return &Provider{OpenAIProvider: openai.NewOpenAIProvider(&c, logger), resolve: c.ChatGPTCredential,
		client: utils.BuildStreamingClient(client), logger: logger, idle: c.NetworkConfig.StreamIdleTimeoutInSeconds, url: baseURL}, nil
}

func failure(message string) *schemas.BifrostError {
	return &schemas.BifrostError{IsBifrostError: true, StatusCode: schemas.Ptr(400), AllowFallbacks: schemas.Ptr(false),
		Error: &schemas.ErrorField{Message: message}, ExtraFields: schemas.BifrostErrorExtraFields{Provider: schemas.ChatGPT}}
}

func (p *Provider) auth(ctx *schemas.BifrostContext, key schemas.Key, model string) (map[string]string, *schemas.BifrostError) {
	if p.resolve == nil || ctx == nil || ctx.Grant() == nil {
		return nil, failure("ChatGPT requires gateway admission and a configured account")
	}
	if access := ctx.Grant().Access(); access != nil {
		if !access.IsModelAllowed(string(schemas.ChatGPT), model) {
			return nil, failure("ChatGPT model is not allowed")
		}
		if ids, restricted := access.KeysForModel(string(schemas.ChatGPT), model); restricted && !slices.Contains(ids, key.ID) {
			return nil, failure("ChatGPT account is not allowed")
		}
	} else if ctx.Grant().Identity() == nil || ctx.Grant().Identity().Presented() || ctx.Grant().Limits() == nil {
		return nil, failure("ChatGPT requires gateway admission")
	}
	token, err := p.resolve(ctx, key)
	if err != nil || token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, failure("ChatGPT credential unavailable; check connection status or reconnect")
	}
	return map[string]string{"Authorization": "Bearer " + token}, nil
}

// prepare retains unknown wire fields and their ordering. The subscription
// route has a narrower contract than API-key Responses; unsupported settings
// are rejected rather than silently changing the requested behavior.
func prepare(body []byte, model string) ([]byte, error) {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, errors.New("ChatGPT requires a JSON object")
	}
	seen := map[string]bool{}
	duplicate := false
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
		if seen[key.String()] {
			duplicate = true
			return false
		}
		seen[key.String()] = true
		return true
	})
	if duplicate {
		return nil, errors.New("ChatGPT rejects duplicate request fields")
	}
	if model == "" || gjson.GetBytes(body, "model").String() != model {
		return nil, errors.New("ChatGPT body model differs from admitted model")
	}
	for _, field := range []string{"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata", "moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier", "temperature", "top_logprobs", "top_p", "truncation", "user", "previous_response_id"} {
		if gjson.GetBytes(body, field).Exists() {
			return nil, fmt.Errorf("ChatGPT does not support %s", field)
		}
	}
	if store := gjson.GetBytes(body, "store"); store.Exists() && store.Raw != "false" && store.Raw != "null" {
		return nil, errors.New("ChatGPT requires store=false")
	}
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		body, _ = sjson.SetBytes(body, "input", []map[string]string{{"role": "user", "content": input.String()}})
		input = gjson.GetBytes(body, "input")
	}
	if !input.IsArray() {
		return nil, errors.New("ChatGPT requires an input array")
	}
	for _, item := range input.Array() {
		if item.Get("role").String() == "system" {
			return nil, errors.New("ChatGPT requires instructions or developer messages, not system messages")
		}
		if item.Get("type").String() == "additional_tools" {
			if err := validateTools(item.Get("tools"), false); err != nil {
				return nil, err
			}
			for _, tool := range item.Get("tools").Array() {
				if tool.Get("type").String() != "namespace" {
					return nil, errors.New("ChatGPT additional_tools must contain namespaces")
				}
			}
		}
	}
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() {
		if err := validateTools(tools, false); err != nil {
			return nil, err
		}
		var namespaces, functions, hosted []json.RawMessage
		for _, tool := range tools.Array() {
			switch tool.Get("type").String() {
			case "function", "custom":
				functions = append(functions, json.RawMessage(tool.Raw))
			case "namespace":
				namespaces = append(namespaces, json.RawMessage(tool.Raw))
			default:
				hosted = append(hosted, json.RawMessage(tool.Raw))
			}
		}
		if len(functions) > 0 {
			for _, tool := range namespaces {
				if gjson.GetBytes(tool, "name").String() == "functions" {
					return nil, errors.New("ChatGPT function namespace collides with an existing namespace")
				}
			}
			namespace, _ := schemas.MarshalSorted(map[string]any{"type": "namespace", "name": "functions", "description": "Client functions", "tools": functions})
			namespaces = append(namespaces, namespace)
		}
		if len(namespaces) > 0 {
			body, _ = sjson.SetBytes(body, "input.-1", map[string]any{"type": "additional_tools", "tools": namespaces})
		}
		if len(hosted) > 0 {
			body, _ = sjson.SetBytes(body, "tools", hosted)
		} else {
			body, _ = sjson.DeleteBytes(body, "tools")
		}
	}
	body, _ = sjson.SetBytes(body, "store", false)
	body, err := sjson.SetBytes(body, "stream", true)
	return body, err
}

func validateTools(tools gjson.Result, inNamespace bool) error {
	if !tools.IsArray() {
		return errors.New("invalid ChatGPT tools")
	}
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case "namespace":
			if inNamespace {
				return errors.New("ChatGPT does not support nested tool namespaces")
			}
			if err := validateTools(tool.Get("tools"), true); err != nil {
				return err
			}
		case "function", "custom":
		case "web_search", "web_search_preview", "web_search_2025_08_26":
			if inNamespace {
				return errors.New("ChatGPT namespaces support only function/custom tools")
			}
		default:
			return fmt.Errorf("ChatGPT does not support tool %s", tool.Get("type").String())
		}
	}
	return nil
}

func (p *Provider) ResponsesStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, finalize func(context.Context), key schemas.Key, request *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	headers, authErr := p.auth(ctx, key, request.Model)
	if authErr != nil {
		return nil, authErr
	}
	if utils.IsLargePayloadPassthroughEnabled(ctx) {
		return nil, failure("ChatGPT does not support large-payload bypass mode")
	}
	r := *request
	body, bodyErr := utils.CheckContextAndGetRequestBody(ctx, &r, func() (utils.RequestBodyWithExtraParams, error) {
		return openai.ToOpenAIResponsesRequest(ctx, &r), nil
	})
	if bodyErr != nil {
		return nil, bodyErr
	}
	body, err := prepare(body, request.Model)
	if err != nil {
		return nil, failure(err.Error())
	}
	r.RawRequestBody = body
	return openai.HandleOpenAIResponsesStreaming(ctx, p.client, p.url+"/responses", &r, headers, nil, p.idle,
		false, false, schemas.ChatGPT, hook, nil, openai.ParseOpenAIError, nil, nil, nil, p.logger, finalize)
}

func (p *Provider) Responses(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	ch, err := p.ResponsesStream(ctx, identityHook, func(context.Context) {}, key, request)
	if err != nil {
		return nil, err
	}
	var result *schemas.BifrostResponsesResponse
	var streamErr *schemas.BifrostError
	items := map[int]schemas.ResponsesMessage{}
	for chunk := range ch {
		if chunk.BifrostError != nil {
			streamErr = chunk.BifrostError
		}
		if r := chunk.BifrostResponsesStreamResponse; r != nil {
			if r.Type == schemas.ResponsesStreamResponseTypeOutputItemDone && r.Item != nil && r.OutputIndex != nil {
				items[*r.OutputIndex] = *r.Item
			}
			if r.Type == schemas.ResponsesStreamResponseTypeCompleted {
				result = r.Response
				if result != nil {
					result.ExtraFields = r.ExtraFields
				}
			}
			if r.Type == schemas.ResponsesStreamResponseTypeIncomplete {
				streamErr = failure("ChatGPT response is incomplete")
			}
		}
	}
	if streamErr != nil {
		return nil, streamErr
	}
	if result == nil {
		return nil, failure("ChatGPT stream ended without response.completed")
	}
	if len(result.Output) == 0 {
		for _, index := range slices.Sorted(maps.Keys(items)) {
			result.Output = append(result.Output, items[index])
		}
	}
	result.ExtraFields.Provider = schemas.ChatGPT
	result.ExtraFields.RequestType = schemas.ResponsesRequest
	return result, nil
}

func identityHook(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return r, e
}

func (p *Provider) ChatCompletion(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	if _, raw := utils.CheckAndGetRawRequestBody(ctx, request); raw {
		return nil, failure("ChatGPT Chat does not support raw-body mode; use Responses")
	}
	response, err := p.Responses(ctx, key, request.ToResponsesRequest())
	if err != nil {
		return nil, err
	}
	return response.ToBifrostChatResponse(), nil
}

func (p *Provider) ChatCompletionStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, finalize func(context.Context), key schemas.Key, request *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	if _, raw := utils.CheckAndGetRawRequestBody(ctx, request); raw {
		return nil, failure("ChatGPT Chat does not support raw-body mode; use Responses")
	}
	indexes := map[int]uint16{}
	convert := func(ctx *schemas.BifrostContext, r *schemas.BifrostResponse, err *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		if r != nil && r.ResponsesStreamResponse != nil {
			event := r.ResponsesStreamResponse
			r.ChatResponse = event.ToBifrostChatResponse()
			for i := range r.ChatResponse.Choices {
				choice := &r.ChatResponse.Choices[i]
				if choice.ChatStreamResponseChoice == nil || choice.Delta == nil || len(choice.Delta.ToolCalls) == 0 {
					continue
				}
				if event.OutputIndex == nil {
					return nil, failure("ChatGPT tool event has no output index")
				}
				index, ok := indexes[*event.OutputIndex]
				if !ok {
					index = uint16(len(indexes))
					indexes[*event.OutputIndex] = index
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

func (p *Provider) PassthroughStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, finalize func(context.Context), key schemas.Key, request *schemas.BifrostPassthroughRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	if request.Method != http.MethodPost || (request.Path != "/responses" && request.Path != "/v1/responses") || request.RawQuery != "" {
		return nil, failure("ChatGPT passthrough supports only POST /responses")
	}
	body, err := prepare(request.Body, request.Model)
	if err != nil {
		return nil, failure(err.Error())
	}
	headers, authErr := p.auth(ctx, key, request.Model)
	if authErr != nil {
		return nil, authErr
	}
	headers["Content-Type"], headers["Accept"] = "application/json", "text/event-stream"
	r := *request
	r.Body, r.SafeHeaders, r.UpstreamURL, r.Path = body, headers, p.url, "/responses"
	return p.OpenAIProvider.PassthroughStream(ctx, hook, finalize, schemas.Key{}, &r)
}

func (p *Provider) Passthrough(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	ch, err := p.PassthroughStream(ctx, identityHook, func(context.Context) {}, key, request)
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
				streamErr = failure("ChatGPT unary response exceeds 32 MiB; use streaming")
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
	for {
		data, err := reader.ReadDataLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, failure("invalid ChatGPT event stream")
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.failed", "error", "response.incomplete":
			return nil, failure("ChatGPT response did not complete")
		case "response.completed":
			response := gjson.GetBytes(data, "response")
			if !response.IsObject() {
				return nil, failure("invalid ChatGPT terminal response")
			}
			result.Body = []byte(response.Raw)
			result.Headers = map[string]string{"Content-Type": "application/json"}
			result.ExtraFields.RequestType = schemas.PassthroughRequest
			return &result, nil
		}
	}
	return nil, failure("ChatGPT stream ended without response.completed")
}

func (p *Provider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	result := &schemas.BifrostListModelsResponse{Data: []schemas.Model{}, ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.ChatGPT}}
	seen := map[string]bool{}
	var lastError *schemas.BifrostError
	for _, key := range keys {
		models, err := p.listModels(ctx, key)
		if err != nil {
			lastError = err
			result.KeyStatuses = append(result.KeyStatuses, schemas.KeyStatus{KeyID: key.ID, Provider: schemas.ChatGPT, Status: schemas.KeyStatusListModelsFailed, Error: err})
			continue
		}
		result.KeyStatuses = append(result.KeyStatuses, schemas.KeyStatus{KeyID: key.ID, Provider: schemas.ChatGPT, Status: schemas.KeyStatusSuccess})
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

func (p *Provider) listModels(ctx *schemas.BifrostContext, key schemas.Key) ([]schemas.Model, *schemas.BifrostError) {
	headers, err := p.auth(ctx, key, "")
	if err != nil {
		return nil, err
	}
	raw, err := p.OpenAIProvider.Passthrough(ctx, schemas.Key{}, &schemas.BifrostPassthroughRequest{Method: http.MethodGet, Path: "/models", UpstreamURL: p.url, SafeHeaders: headers})
	if err != nil {
		return nil, err
	}
	models := gjson.GetBytes(raw.Body, "models")
	if raw.StatusCode != 200 || !models.IsArray() {
		return nil, failure("ChatGPT model catalog unavailable")
	}
	result := []schemas.Model{}
	for _, model := range models.Array() {
		id := model.Get("slug").String()
		if id == "" || model.Get("visibility").String() != "list" || !key.Models.IsAllowed(id) || key.BlacklistedModels.IsBlocked(id) || (ctx.Grant().Access() != nil && !ctx.Grant().Access().IsModelAllowed(string(schemas.ChatGPT), id)) {
			continue
		}
		result = append(result, schemas.Model{ID: "chatgpt/" + id, Name: schemas.Ptr(model.Get("display_name").String()), OwnedBy: schemas.Ptr("chatgpt")})
	}
	return result, nil
}
