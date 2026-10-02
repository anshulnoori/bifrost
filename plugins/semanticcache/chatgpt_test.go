package semanticcache

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestChatGPTCacheIsolatesAccountsAndRechecksAdmission(t *testing.T) {
	store := newCodexTestStore()
	plugin := newCodexPlugin(t, store, false)
	plugin.SetCodexCacheScopeResolver(fixedCodexResolver("plan/account-a", "key-a"))
	request := codexRequest("shared prompt")
	request.ResponsesRequest.Provider = schemas.ChatGPT
	request.ResponsesRequest.Model = "gpt-6.1-sol"
	cold := codexContext(t, CacheTypeDirect, "key-a")
	_, hit, err := plugin.PreLLMHook(cold, request)
	require.NoError(t, err)
	require.Nil(t, hit)
	_, _, err = plugin.PostLLMHook(cold, codexResponse(schemas.ChatGPT, "gpt-6.1-sol"), nil)
	require.NoError(t, err)
	plugin.WaitForPendingOperations()
	require.Len(t, store.addIDs, 1)
	_, hit, err = plugin.PreLLMHook(codexContext(t, CacheTypeDirect, ""), request)
	require.NoError(t, err)
	require.NotNil(t, hit)
	plugin.SetCodexCacheScopeResolver(fixedCodexResolver("plan/account-b", "key-b"))
	_, hit, err = plugin.PreLLMHook(codexContext(t, CacheTypeDirect, "key-b"), request)
	require.NoError(t, err)
	require.Nil(t, hit)
	plugin.SetCodexCacheScopeResolver(nil)
	_, hit, err = plugin.PreLLMHook(codexContext(t, CacheTypeDirect, "key-a"), request)
	require.NoError(t, err)
	require.Nil(t, hit)
}
