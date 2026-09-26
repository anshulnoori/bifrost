package semanticcache

import (
	"context"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"
)

const codexTestModel = "gpt-5.3-codex"

type codexTestStore struct {
	*observableStore
	mu             sync.Mutex
	beforeGetChunk func()
	nearest        []vectorstore.SearchResult
}

func newCodexTestStore() *codexTestStore {
	return &codexTestStore{observableStore: newObservableStore()}
}

func (s *codexTestStore) GetChunk(ctx context.Context, namespace, id string) (vectorstore.SearchResult, error) {
	s.mu.Lock()
	before := s.beforeGetChunk
	s.mu.Unlock()
	result, err := s.observableStore.GetChunk(ctx, namespace, id)
	if before != nil {
		before()
	}
	return result, err
}

func (s *codexTestStore) GetNearest(context.Context, string, []float32, []vectorstore.Query, []string, float64, int64) ([]vectorstore.SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]vectorstore.SearchResult(nil), s.nearest...), nil
}

func newCodexPlugin(t *testing.T, store vectorstore.VectorStore, semantic bool) *Plugin {
	t.Helper()
	plugin := newTestPlugin(t, store)
	if semantic {
		plugin.config.Provider = schemas.OpenAI
		plugin.config.EmbeddingModel = "embedding-test"
		plugin.config.Dimension = 3
		plugin.SetEmbeddingRequestExecutor(func(*schemas.BifrostContext, *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
			return &schemas.BifrostEmbeddingResponse{Data: []schemas.EmbeddingData{{Embedding: schemas.EmbeddingStruct{EmbeddingArray: []float64{1, 0, 0}}}}}, nil
		})
	} else {
		plugin.config.Provider = ""
		plugin.config.EmbeddingModel = ""
		plugin.config.Dimension = 1
	}
	return plugin
}

func codexRequest(prompt string) *schemas.BifrostRequest {
	request := CreateBasicResponsesRequest(prompt, 0.2, 100)
	request.Provider = schemas.Codex
	request.Model = codexTestModel
	return &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: request}
}

func codexResponse(provider schemas.ModelProvider, model string) *schemas.BifrostResponse {
	id := "response-id"
	return &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{
		ID:    &id,
		Model: model,
		ExtraFields: schemas.BifrostResponseExtraFields{
			Provider:               provider,
			OriginalModelRequested: model,
			RequestType:            schemas.ResponsesRequest,
		},
	}}
}

func codexContext(t *testing.T, cacheType CacheType, selectedKey string) *schemas.BifrostContext {
	t.Helper()
	ctx := CreateContextWithCacheKeyAndType(t, "codex", cacheType)
	if selectedKey != "" {
		ctx.SetValue(schemas.BifrostContextKeySelectedKeyID, selectedKey)
	}
	return ctx
}

func fixedCodexResolver(scope string, keys ...string) CodexCacheScopeResolver {
	return func(*schemas.BifrostContext, schemas.ModelProvider, string) (string, []string, error) {
		return scope, keys, nil
	}
}

func TestCodexMissingScopeResolverFailsClosed(t *testing.T) {
	plugin := &Plugin{config: &Config{DefaultCacheKey: "shared"}}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(CacheKey, "shared")
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "codex-isolation")
	req := &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: schemas.Codex, Model: "gpt-5.3-codex"}}
	stale := plugin.createCacheState("codex-isolation")
	stale.ParamsHash = "must-be-cleared"
	got, short, err := plugin.PreLLMHook(ctx, req)
	if got != req || short != nil || err != nil {
		t.Fatalf("Codex cache lookup did not fail closed: %v %v", short, err)
	}
	if plugin.getCacheState("codex-isolation") != nil {
		t.Fatal("stale request state survived failed Codex authorization")
	}
	res := &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Codex, RequestType: schemas.ResponsesRequest}}}
	response, bfErr, err := plugin.PostLLMHook(ctx, res, nil)
	if response != res || bfErr != nil || err != nil {
		t.Fatalf("Codex cache write did not fail closed: %v %v", bfErr, err)
	}
}

func TestCodexDirectColdWriteThenWarmHit(t *testing.T) {
	store := newCodexTestStore()
	plugin := newCodexPlugin(t, store, false)
	plugin.SetCodexCacheScopeResolver(fixedCodexResolver("account-a", "key-a"))

	coldCtx := codexContext(t, CacheTypeDirect, "key-a")
	request := codexRequest("direct prompt")
	if _, hit, err := plugin.PreLLMHook(coldCtx, request); err != nil || hit != nil {
		t.Fatalf("cold lookup = (%v, %v), want miss", hit, err)
	}
	if _, _, err := plugin.PostLLMHook(coldCtx, codexResponse(schemas.Codex, codexTestModel), nil); err != nil {
		t.Fatalf("cold PostLLMHook failed: %v", err)
	}
	plugin.WaitForPendingOperations()
	if len(store.addIDs) != 1 {
		t.Fatalf("cold request wrote %d entries, want 1", len(store.addIDs))
	}

	warmCtx := codexContext(t, CacheTypeDirect, "")
	if _, hit, err := plugin.PreLLMHook(warmCtx, codexRequest("direct prompt")); err != nil || hit == nil {
		t.Fatalf("warm lookup = (%v, %v), want hit", hit, err)
	}
	// Cache replays do not select an upstream key. Post must still take its
	// short-circuit path and must not duplicate the entry.
	if _, _, err := plugin.PostLLMHook(warmCtx, codexResponse(schemas.Codex, codexTestModel), nil); err != nil {
		t.Fatalf("warm PostLLMHook failed: %v", err)
	}
	plugin.WaitForPendingOperations()
	if len(store.addIDs) != 1 {
		t.Fatalf("warm hit rewrote cache: add IDs = %v", store.addIDs)
	}
}

func TestCodexSemanticColdWriteThenWarmHit(t *testing.T) {
	store := newCodexTestStore()
	plugin := newCodexPlugin(t, store, true)
	plugin.SetCodexCacheScopeResolver(fixedCodexResolver("account-a", "key-a"))

	coldCtx := codexContext(t, CacheTypeSemantic, "key-a")
	if _, hit, err := plugin.PreLLMHook(coldCtx, codexRequest("semantic prompt")); err != nil || hit != nil {
		t.Fatalf("cold lookup = (%v, %v), want miss", hit, err)
	}
	if _, _, err := plugin.PostLLMHook(coldCtx, codexResponse(schemas.Codex, codexTestModel), nil); err != nil {
		t.Fatalf("cold PostLLMHook failed: %v", err)
	}
	plugin.WaitForPendingOperations()
	if len(store.addIDs) != 1 {
		t.Fatalf("cold request wrote %d entries, want 1", len(store.addIDs))
	}
	store.observableStore.mu.Lock()
	stored := store.chunks[store.addIDs[0]]
	store.observableStore.mu.Unlock()
	store.mu.Lock()
	store.nearest = []vectorstore.SearchResult{stored}
	store.mu.Unlock()

	warmCtx := codexContext(t, CacheTypeSemantic, "")
	if _, hit, err := plugin.PreLLMHook(warmCtx, codexRequest("similar semantic prompt")); err != nil || hit == nil {
		t.Fatalf("warm semantic lookup = (%v, %v), want hit", hit, err)
	}
}

func TestCodexDifferentVirtualKeyOrAccountScopeMissesDirectEntry(t *testing.T) {
	store := newCodexTestStore()
	plugin := newCodexPlugin(t, store, false)
	plugin.SetCodexCacheScopeResolver(fixedCodexResolver("vk-a/account-a", "key-a"))
	ctx := codexContext(t, CacheTypeDirect, "key-a")
	if _, _, err := plugin.PreLLMHook(ctx, codexRequest("isolated prompt")); err != nil {
		t.Fatal(err)
	}
	_, _, _ = plugin.PostLLMHook(ctx, codexResponse(schemas.Codex, codexTestModel), nil)
	plugin.WaitForPendingOperations()

	for _, scope := range []string{"vk-a/account-b", "vk-b/account-a"} {
		plugin.SetCodexCacheScopeResolver(fixedCodexResolver(scope, "key-b"))
		if _, hit, err := plugin.PreLLMHook(codexContext(t, CacheTypeDirect, "key-b"), codexRequest("isolated prompt")); err != nil || hit != nil {
			t.Fatalf("scope %q lookup = (%v, %v), want miss", scope, hit, err)
		}
	}
}

func TestCodexDisconnectDuringDirectReadRechecksScope(t *testing.T) {
	store := newCodexTestStore()
	plugin := newCodexPlugin(t, store, false)
	scope := "account-a"
	plugin.SetCodexCacheScopeResolver(func(*schemas.BifrostContext, schemas.ModelProvider, string) (string, []string, error) {
		return scope, []string{"key-a"}, nil
	})
	ctx := codexContext(t, CacheTypeDirect, "key-a")
	_, _, _ = plugin.PreLLMHook(ctx, codexRequest("disconnect prompt"))
	_, _, _ = plugin.PostLLMHook(ctx, codexResponse(schemas.Codex, codexTestModel), nil)
	plugin.WaitForPendingOperations()
	store.mu.Lock()
	store.beforeGetChunk = func() { scope = "disconnected" }
	store.mu.Unlock()

	if _, hit, err := plugin.PreLLMHook(codexContext(t, CacheTypeDirect, "key-a"), codexRequest("disconnect prompt")); err != nil || hit != nil {
		t.Fatalf("lookup after disconnect = (%v, %v), want miss", hit, err)
	}
}

func TestCodexReconnectDuringUpstreamDropsWrite(t *testing.T) {
	store := newCodexTestStore()
	plugin := newCodexPlugin(t, store, false)
	scope := "account-a"
	plugin.SetCodexCacheScopeResolver(func(*schemas.BifrostContext, schemas.ModelProvider, string) (string, []string, error) {
		return scope, []string{"key-a"}, nil
	})
	ctx := codexContext(t, CacheTypeDirect, "key-a")
	_, _, _ = plugin.PreLLMHook(ctx, codexRequest("reconnect prompt"))
	scope = "account-b"
	_, _, _ = plugin.PostLLMHook(ctx, codexResponse(schemas.Codex, codexTestModel), nil)
	plugin.WaitForPendingOperations()
	if len(store.addIDs) != 0 {
		t.Fatalf("scope-changing request wrote cache: %v", store.addIDs)
	}
}

func TestCodexWrongSuccessfulKeyDropsWrite(t *testing.T) {
	store := newCodexTestStore()
	plugin := newCodexPlugin(t, store, false)
	plugin.SetCodexCacheScopeResolver(fixedCodexResolver("account-a", "eligible"))
	ctx := codexContext(t, CacheTypeDirect, "wrong-key")
	_, _, _ = plugin.PreLLMHook(ctx, codexRequest("wrong key prompt"))
	_, _, _ = plugin.PostLLMHook(ctx, codexResponse(schemas.Codex, codexTestModel), nil)
	plugin.WaitForPendingOperations()
	if len(store.addIDs) != 0 {
		t.Fatalf("response from ineligible key wrote cache: %v", store.addIDs)
	}
}

func TestCodexFallbacksAndEarlyReturnCannotReuseStaleState(t *testing.T) {
	tests := []struct {
		name             string
		preRequest       *schemas.BifrostRequest
		responseProvider schemas.ModelProvider
		responseModel    string
		earlyReturn      bool
	}{
		{name: "Codex request falls back to OpenAI", preRequest: codexRequest("fallback out"), responseProvider: schemas.OpenAI, responseModel: codexTestModel},
		{name: "OpenAI request falls back to Codex", preRequest: &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: CreateBasicResponsesRequest("fallback in", 0.2, 100)}, responseProvider: schemas.Codex, responseModel: codexTestModel},
		{name: "Codex Pre early return", preRequest: codexRequest("early return"), responseProvider: schemas.Codex, responseModel: codexTestModel, earlyReturn: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newCodexTestStore()
			plugin := newCodexPlugin(t, store, false)
			plugin.SetCodexCacheScopeResolver(fixedCodexResolver("account-a", "key-a"))
			ctx := codexContext(t, CacheTypeDirect, "key-a")
			requestID, _ := ctx.Value(schemas.BifrostContextKeyRequestID).(string)
			stale := plugin.createCacheState(requestID)
			stale.ParamsHash = "stale"
			stale.CodexProvider = string(schemas.Codex)
			stale.CodexModel = codexTestModel
			stale.CodexScope = "account-a"
			stale.CodexEligibleKeyIDs = map[string]struct{}{"key-a": {}}
			if test.earlyReturn {
				ctx.SetValue(CacheKey, "")
				plugin.config.DefaultCacheKey = ""
			}
			if _, hit, err := plugin.PreLLMHook(ctx, test.preRequest); err != nil || hit != nil {
				t.Fatalf("PreLLMHook = (%v, %v)", hit, err)
			}
			_, _, _ = plugin.PostLLMHook(ctx, codexResponse(test.responseProvider, test.responseModel), nil)
			plugin.WaitForPendingOperations()
			if len(store.addIDs) != 0 {
				t.Fatalf("fallback/early-return wrote cache using stale state: %v", store.addIDs)
			}
		})
	}
}
