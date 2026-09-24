package semanticcache

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"
)

func TestVirtualKeyCacheValkey(t *testing.T) {
	if os.Getenv("BIFROST_TEST_VALKEY") != "1" {
		t.Skip("requires disposable Valkey Search, BIFROST_TEST_VALKEY=1")
	}
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	store, err := vectorstore.NewVectorStore(context.Background(), &vectorstore.Config{
		Enabled: true, Type: vectorstore.VectorStoreTypeRedis, Config: getRedisConfigFromEnv(),
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(context.Background(), "ScopedCacheTest")
	if err := store.DeleteNamespace(context.Background(), "ScopedCacheTest"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.DeleteNamespace(context.Background(), "ScopedCacheTest"); err != nil {
			t.Error(err)
		}
	}()
	p, err := Init(context.Background(), &Config{
		Dimension: 1, ScopeByVirtualKey: true, DefaultCacheKey: "shared",
		VectorStoreNamespace: "ScopedCacheTest",
	}, logger, store)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	request := &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest,
		ChatRequest: CreateBasicChatRequest("Synthetic private response", 0, 10)}
	ctx := newBaseTestContext()
	ctx.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, "tenant-a")
	_, hit, err := p.PreLLMHook(ctx, request)
	if err != nil || hit != nil {
		t.Fatalf("initial miss: hit=%v err=%v", hit != nil, err)
	}
	response := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{
		ID: "private-a", Choices: []schemas.BifrostResponseChoice{{Index: 0,
			ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{Message: &schemas.ChatMessage{
				Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: bifrost.Ptr("only-a")},
			}},
		}}, ExtraFields: schemas.BifrostResponseExtraFields{
			Provider: schemas.OpenAI, OriginalModelRequested: request.ChatRequest.Model, RequestType: schemas.ChatCompletionRequest,
		},
	}}
	if _, _, err := p.PostLLMHook(ctx, response, nil); err != nil {
		t.Fatal(err)
	}
	WaitForCache(p)
	for _, id := range []string{"tenant-a", "tenant-b", ""} {
		c := newBaseTestContext()
		c.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, id)
		_, hit, err := p.PreLLMHook(c, request)
		if err != nil || (hit != nil) != (id == "tenant-a") {
			t.Fatalf("tenant %q: hit=%v err=%v", id, hit != nil, err)
		}
		if hit != nil && *hit.Response.ChatResponse.Choices[0].Message.Content.ContentStr != "only-a" {
			t.Fatal("cached response content changed")
		}
	}
}

func TestVirtualKeyCacheScope(t *testing.T) {
	var config Config
	if err := json.Unmarshal([]byte(`{"default_cache_key":"shared","scope_by_virtual_key":true}`), &config); err != nil {
		t.Fatal(err)
	}
	plugin := &Plugin{config: &config}
	resolve := func(id interface{}, key string) (string, bool) {
		ctx := newBaseTestContext()
		if id != nil {
			ctx.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, id)
		}
		ctx.SetValue(CacheKey, key)
		return plugin.resolveCacheKey(ctx)
	}
	a, ok := resolve("vk-a", "")
	if !ok || a == "shared" {
		t.Fatal("authenticated cache key must be scoped")
	}
	b, ok := resolve("vk-b", "")
	if !ok || a == b {
		t.Fatal("different authenticated virtual keys must not share cached responses")
	}
	again, _ := resolve("vk-a", "shared")
	if again != a {
		t.Fatal("same stable ID and effective key must retain the same namespace")
	}
	forged, _ := resolve("vk-b", a)
	if forged == a {
		t.Fatal("caller-supplied cache key must not impersonate another virtual key")
	}
	for _, id := range []interface{}{nil, "", 12} {
		if key, ok := resolve(id, "caller-key"); ok || key != "" {
			t.Fatal("missing or malformed authenticated identity must bypass caching")
		}
	}
	left, _ := resolve("a:b", "c")
	right, _ := resolve("a", "b:c")
	if left == right {
		t.Fatal("ambiguous concatenation must not collide")
	}
}

// TestDefaultCacheKey_CachesWithoutPerRequestKey verifies that when DefaultCacheKey
// is configured, requests without an explicit cache key are cached automatically.
func TestDefaultCacheKey_CachesWithoutPerRequestKey(t *testing.T) {
	t.Parallel()
	config := getDefaultTestConfig()
	config.DefaultCacheKey = keyForTest(t, "test-default-key")

	setup := NewTestSetupWithConfig(t, config)
	defer setup.Cleanup()

	// Context with NO per-request cache key
	ctx := newBaseTestContext()

	testRequest := CreateBasicChatRequest("What is Bifrost? Answer in one short sentence.", 0.7, 50)

	t.Log("Making first request without per-request cache key (should use default and be cached)...")
	response1, err1 := setup.Client.ChatCompletionRequest(ctx, testRequest)
	if err1 != nil {
		t.Skipf("upstream request error, skipping test: %v", err1)
	}

	if response1 == nil || len(response1.Choices) == 0 || response1.Choices[0].Message.Content.ContentStr == nil {
		t.Fatal("First response is invalid")
	}

	// First request should NOT be a cache hit
	AssertNoCacheHit(t, &schemas.BifrostResponse{ChatResponse: response1})

	WaitForCache(setup.Plugin)

	t.Log("Making second identical request without per-request cache key (should hit cache)...")
	ctx2 := newBaseTestContext()
	response2, err2 := setup.Client.ChatCompletionRequest(ctx2, testRequest)
	if err2 != nil {
		if err2.Error != nil {
			t.Fatalf("Second request failed: %v", err2.Error.Message)
		}
		t.Fatalf("Second request failed: %v", err2)
	}

	AssertCacheHit(t, &schemas.BifrostResponse{ChatResponse: response2}, string(CacheTypeDirect))
	t.Log("Default cache key correctly enabled caching without per-request key")
}

// TestDefaultCacheKey_PerRequestKeyOverridesDefault verifies that an explicit
// per-request cache key takes precedence over the configured default.
func TestDefaultCacheKey_PerRequestKeyOverridesDefault(t *testing.T) {
	t.Parallel()
	config := getDefaultTestConfig()
	config.DefaultCacheKey = keyForTest(t, "test-default-key")

	setup := NewTestSetupWithConfig(t, config)
	defer setup.Cleanup()

	testRequest := CreateBasicChatRequest("What is the capital of France?", 0.5, 50)

	// Cache with the default key (no per-request key)
	ctx1 := newBaseTestContext()
	_, err1 := setup.Client.ChatCompletionRequest(ctx1, testRequest)
	if err1 != nil {
		t.Skipf("upstream request error, skipping test: %v", err1)
	}

	WaitForCache(setup.Plugin)

	// Verify the cache was actually populated with the default key
	ctxDefault2 := newBaseTestContext()
	responseDefault2, errDefault2 := setup.Client.ChatCompletionRequest(ctxDefault2, testRequest)
	if errDefault2 != nil {
		if errDefault2.Error != nil {
			t.Fatalf("Default-key verification request failed: %v", errDefault2.Error.Message)
		}
		t.Fatalf("Default-key verification request failed: %v", errDefault2)
	}
	AssertCacheHit(t, &schemas.BifrostResponse{ChatResponse: responseDefault2}, string(CacheTypeDirect))

	// Same request but with a DIFFERENT per-request key — should miss
	ctx2 := CreateContextWithCacheKey(t, "override-key")
	response2, err2 := setup.Client.ChatCompletionRequest(ctx2, testRequest)
	if err2 != nil {
		if err2.Error != nil {
			t.Fatalf("Second request failed: %v", err2.Error.Message)
		}
		t.Fatalf("Second request failed: %v", err2)
	}

	AssertNoCacheHit(t, &schemas.BifrostResponse{ChatResponse: response2})
	t.Log("Per-request cache key correctly overrides default (different namespace = cache miss)")
}

// TestDefaultCacheKey_EmptyDefault_NoCaching verifies that when DefaultCacheKey
// is empty (default zero value), requests without a per-request key bypass caching.
func TestDefaultCacheKey_EmptyDefault_NoCaching(t *testing.T) {
	t.Parallel()
	config := getDefaultTestConfig()
	// DefaultCacheKey is intentionally left empty (zero value)

	setup := NewTestSetupWithConfig(t, config)
	defer setup.Cleanup()

	ctx := newBaseTestContext()

	testRequest := CreateBasicChatRequest("What is deep learning", 0.7, 50)

	t.Log("Making first request without any cache key and no default (should not cache)...")
	response1, err1 := setup.Client.ChatCompletionRequest(ctx, testRequest)
	if err1 != nil {
		t.Skipf("upstream request error, skipping test: %v", err1)
	}

	AssertNoCacheHit(t, &schemas.BifrostResponse{ChatResponse: response1})

	WaitForCache(setup.Plugin)

	t.Log("Making second identical request (should still not cache)...")
	ctx2 := newBaseTestContext()
	response2, err2 := setup.Client.ChatCompletionRequest(ctx2, testRequest)
	if err2 != nil {
		if err2.Error != nil {
			t.Fatalf("Second request failed: %v", err2.Error.Message)
		}
		t.Fatalf("Second request failed: %v", err2)
	}

	AssertNoCacheHit(t, &schemas.BifrostResponse{ChatResponse: response2})
	t.Log("Empty default cache key correctly preserves opt-in behavior")
}
