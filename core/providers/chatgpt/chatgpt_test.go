package chatgpt

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type testLogger struct{ schemas.Logger }

func (testLogger) Debug(string, ...any) {}
func (testLogger) Warn(string, ...any)  {}
func (testLogger) Info(string, ...any)  {}
func (testLogger) Error(string, ...any) {}

type testAccess struct{ schemas.Access }

func (testAccess) IsModelAllowed(p, model string) bool {
	return p == "chatgpt" && (model == "" || model == "gpt-6.1-sol")
}
func (testAccess) KeysForModel(string, string) ([]string, bool) { return []string{"account-A"}, true }

type testGrant struct{ schemas.Grant }

func (testGrant) Access() schemas.Access { return testAccess{} }
func admitted() *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetGrant(testGrant{})
	return ctx
}

func TestPrepareUsesPlanWireFormatWithoutMutatingCaller(t *testing.T) {
	input := `{"model":"gpt-6.1-sol","input":"hello","tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"future":{"keep":17},"stream":false}`
	body, err := prepare([]byte(input), "gpt-6.1-sol")
	require.NoError(t, err)
	require.Equal(t, "hello", gjson.GetBytes(body, "input.0.content").String())
	require.Equal(t, "additional_tools", gjson.GetBytes(body, "input.1.type").String())
	require.Equal(t, "namespace", gjson.GetBytes(body, "input.1.tools.0.type").String())
	require.Equal(t, "shell", gjson.GetBytes(body, "input.1.tools.0.tools.0.name").String())
	require.False(t, gjson.GetBytes(body, "tools").Exists())
	require.False(t, gjson.GetBytes(body, "store").Bool())
	require.True(t, gjson.GetBytes(body, "stream").Bool())
	require.Equal(t, int64(17), gjson.GetBytes(body, "future.keep").Int())
	require.True(t, strings.Contains(input, `"stream":false`))
	_, err = prepare([]byte(`{"model":"gpt-6.1-sol","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"mcp","tools":[{"type":"custom","name":"patch"}]}]}]}`), "gpt-6.1-sol")
	require.NoError(t, err)
}

func TestPrepareRejectsUnsupportedSemantics(t *testing.T) {
	for _, input := range []string{
		`{"model":"gpt-6.1-sol","model":"other","input":[]}`,
		`{"model":"other","input":[]}`,
		`{"model":"gpt-6.1-sol","input":[],"store":true}`,
		`{"model":"gpt-6.1-sol","input":[],"previous_response_id":"prior"}`,
		`{"model":"gpt-6.1-sol","input":[],"metadata":{}}`,
		`{"model":"gpt-6.1-sol","input":[{"type":"message","role":"system","content":"x"}]}`,
		`{"model":"gpt-6.1-sol","input":[],"tools":[{"type":"tool_search"}]}`,
		`{"model":"gpt-6.1-sol","input":[],"tools":[{"type":"mcp","server_url":"https://other"}]}`,
		`{"model":"gpt-6.1-sol","input":[{"type":"additional_tools","tools":[{"type":"function","name":"ungrouped"}]}]}`,
	} {
		_, err := prepare([]byte(input), "gpt-6.1-sol")
		require.Error(t, err, input)
	}
}

func TestAccountAdmissionPrecedesCredentialResolution(t *testing.T) {
	calls := 0
	p, err := New(&schemas.ProviderConfig{ChatGPTCredential: func(*schemas.BifrostContext, schemas.Key) (string, error) { calls++; return "private-token", nil }}, testLogger{})
	require.NoError(t, err)
	_, bfErr := p.auth(admitted(), schemas.Key{ID: "account-B"}, "gpt-6.1-sol")
	require.NotNil(t, bfErr)
	require.Equal(t, schemas.ChatGPT, bfErr.ExtraFields.Provider)
	require.Equal(t, 0, calls)
	headers, bfErr := p.auth(admitted(), schemas.Key{ID: "account-A"}, "gpt-6.1-sol")
	require.Nil(t, bfErr)
	require.Equal(t, "Bearer private-token", headers["Authorization"])
	require.Empty(t, headers["ChatGPT-Account-ID"])
	require.Equal(t, 1, calls)
}

func TestPlanResponsesAndAccountCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer subscription-token", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("ChatGPT-Account-ID"))
		if r.URL.Path == "/models" {
			require.Empty(t, r.URL.RawQuery)
			fmt.Fprint(w, `{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT 6.1 Sol","visibility":"list"},{"slug":"hidden","visibility":"hidden"},{"slug":"not-allowed","visibility":"list"}]}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.True(t, gjson.GetBytes(body, "stream").Bool())
		require.False(t, gjson.GetBytes(body, "store").Bool())
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-test\",\"status\":\"completed\",\"model\":\"gpt-6.1-sol\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"pong\"}]}],\"usage\":{\"input_tokens\":23,\"output_tokens\":7,\"total_tokens\":30}}}\n\n")
	}))
	defer server.Close()
	p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{AllowPrivateNetwork: true}, ChatGPTCredential: func(*schemas.BifrostContext, schemas.Key) (string, error) { return "subscription-token", nil }}, testLogger{})
	require.NoError(t, err)
	p.url = server.URL
	key := schemas.Key{ID: "account-A", Models: schemas.WhiteList{"*"}}
	response, bfErr := p.Responses(admitted(), key, &schemas.BifrostResponsesRequest{Provider: schemas.ChatGPT, Model: "gpt-6.1-sol", Input: []schemas.ResponsesMessage{{Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser), Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("ping")}}}})
	require.Nil(t, bfErr)
	require.Equal(t, schemas.Ptr("resp-test"), response.ID)
	require.Equal(t, schemas.ChatGPT, response.ExtraFields.Provider)
	models, bfErr := p.ListModels(admitted(), []schemas.Key{key}, &schemas.BifrostListModelsRequest{})
	require.Nil(t, bfErr)
	require.Len(t, models.Data, 1)
	require.Equal(t, "chatgpt/gpt-6.1-sol", models.Data[0].ID)
}

func TestProviderCannotBeRedirected(t *testing.T) {
	_, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: "https://attacker.invalid"}}, testLogger{})
	require.Error(t, err)
}
