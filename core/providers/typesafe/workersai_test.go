package typesafe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	schemas "github.com/maximhq/bifrost/core/schemas"
)

// workersAIServer fakes the Workers AI REST endpoint for one account. It
// records each request and answers with handler's status and body.
func workersAIServer(t *testing.T, status int, body string) (*httptest.Server, *[]*http.Request, *[][]byte) {
	t.Helper()
	var requests []*http.Request
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		requests = append(requests, r.Clone(context.Background()))
		bodies = append(bodies, data)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &requests, &bodies
}

func newWorkersAIProvider(t *testing.T, base string) *TypesafeProvider {
	t.Helper()
	provider, err := NewCloudflareProvider(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: base, DefaultRequestTimeoutInSeconds: 10}}, noopLogger{})
	if err != nil {
		t.Fatalf("constructor failed: %v", err)
	}
	return provider
}

func clefRequest(model string) *schemas.BifrostDecisionRequest {
	return &schemas.BifrostDecisionRequest{
		Provider: schemas.Cloudflare,
		Model:    model,
		State:    "Canvas: Problem set 4 due Friday",
		Questions: map[string]schemas.DecisionQuestion{
			"is_task":  {Kind: schemas.DecisionKindNoul, Instructions: "Is this something the student must do?"},
			"type":     {Kind: schemas.DecisionKindChoice, Instructions: "Which task type?", Criteria: map[string]any{"assignment": "graded work", "reading": "reading only"}},
			"priority": {Kind: schemas.DecisionKindScore, Instructions: "How urgent?", Criteria: []any{"low", "medium", "high"}},
		},
	}
}

// Clef answers arrive inside Cloudflare's REST envelope; the provider must call
// the account's /ai/run/@cf/cloudflare/<model> route with the bare System One
// model name and unwrap result into typed answers.
func TestCloudflareClefDecision(t *testing.T) {
	envelope := `{"result":{"model":"clef","answers":{` +
		`"is_task":{"type":"noul","noul":0.93},` +
		`"type":{"type":"choice","choice":"assignment","confidence":0.8,"probabilities":{"assignment":0.9,"reading":0.1}},` +
		`"priority":{"type":"score","score":2.25,"confidence":0.6,"legend":{"1":"low","2":"medium","3":"high"},"probabilities":{"1":0.05,"2":0.65,"3":0.3}}},` +
		`"usage":{"input_tokens":311,"output_tokens":0}},"success":true,"errors":[],"messages":[]}`
	server, requests, bodies := workersAIServer(t, http.StatusOK, envelope)
	provider := newWorkersAIProvider(t, server.URL+"/client/v4/accounts/acct-1/ai/run/")

	resp, bifrostErr := provider.Decision(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		schemas.Key{Value: *schemas.NewSecretVar("cf-token")}, clefRequest("clef"))
	if bifrostErr != nil {
		t.Fatalf("decision failed: %+v", bifrostErr.Error)
	}
	if len(*requests) != 1 {
		t.Fatalf("upstream calls = %d", len(*requests))
	}
	got := (*requests)[0]
	if got.Method != http.MethodPost || got.URL.Path != "/client/v4/accounts/acct-1/ai/run/@cf/cloudflare/clef" {
		t.Fatalf("upstream request = %s %s", got.Method, got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer cf-token" {
		t.Fatalf("authorization header = %q", got.Header.Get("Authorization"))
	}
	var sent struct {
		Model     string                     `json:"model"`
		State     string                     `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal((*bodies)[0], &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "clef" || sent.State != "Canvas: Problem set 4 due Friday" || len(sent.Questions) != 3 {
		t.Fatalf("System One body = %s", (*bodies)[0])
	}
	if !strings.Contains(string(sent.Questions["type"]), `"type":"choice"`) {
		t.Fatalf("question type not native: %s", sent.Questions["type"])
	}

	if resp.Model != "clef" || resp.Answers["is_task"].Value != 0.93 || resp.Answers["type"].Value != "assignment" || resp.Answers["priority"].Value != 2.25 {
		t.Fatalf("answers = %+v", resp.Answers)
	}
	if resp.Answers["type"].Probabilities["reading"] != 0.1 || *resp.Answers["priority"].Confidence != 0.6 {
		t.Fatalf("probabilities/confidence lost: %+v", resp.Answers)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 311 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

// Cloudflare reports failures as {"errors":[{"code","message"}],"success":false};
// the status and the first message must survive.
func TestCloudflareClefErrorEnvelope(t *testing.T) {
	server, _, _ := workersAIServer(t, http.StatusBadRequest, `{"result":null,"success":false,"errors":[{"code":5006,"message":"Error: model must match pattern ^(clef|clef-flash)$"}],"messages":[]}`)
	provider := newWorkersAIProvider(t, server.URL+"/client/v4/accounts/acct-1/ai/run")
	_, bifrostErr := provider.Decision(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
		schemas.Key{Value: *schemas.NewSecretVar("cf-token")}, clefRequest("clef"))
	if bifrostErr == nil || bifrostErr.StatusCode == nil || *bifrostErr.StatusCode != 400 || bifrostErr.Error == nil || !strings.Contains(bifrostErr.Error.Message, "must match pattern") {
		t.Fatalf("error = %+v", bifrostErr)
	}
}

// The model is part of the upstream path, so anything but a plain model name is
// rejected before a request is made.
func TestCloudflareRejectsPathLikeModels(t *testing.T) {
	server, requests, _ := workersAIServer(t, http.StatusOK, `{}`)
	provider := newWorkersAIProvider(t, server.URL)
	for _, model := range []string{"", "../clef", "clef/../../x", "@cf/cloudflare/clef", "clef?x=1", "Clef#a"} {
		if _, bifrostErr := provider.Decision(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline),
			schemas.Key{Value: *schemas.NewSecretVar("cf-token")}, clefRequest(model)); bifrostErr == nil {
			t.Fatalf("accepted model %q", model)
		}
	}
	if len(*requests) != 0 {
		t.Fatalf("rejected models reached upstream %d times", len(*requests))
	}
}

func TestCloudflareProviderIdentityAndCatalog(t *testing.T) {
	if _, err := NewCloudflareProvider(&schemas.ProviderConfig{}, noopLogger{}); err == nil {
		t.Fatal("cloudflare provider must require its account base URL")
	}
	provider := newWorkersAIProvider(t, "https://api.cloudflare.com/client/v4/accounts/acct-1/ai/run")
	if provider.GetProviderKey() != schemas.Cloudflare {
		t.Fatalf("provider key = %s", provider.GetProviderKey())
	}
	if _, bifrostErr := provider.ChatCompletion(nil, schemas.Key{}, &schemas.BifrostChatRequest{}); bifrostErr == nil || bifrostErr.ExtraFields.Provider != schemas.Cloudflare {
		t.Fatalf("unsupported error = %+v", bifrostErr)
	}
	resp, bifrostErr := provider.listModelsByKey(nil, schemas.Key{Models: []string{"*"}}, &schemas.BifrostListModelsRequest{})
	if bifrostErr != nil {
		t.Fatal(bifrostErr)
	}
	var ids []string
	for _, model := range resp.Data {
		ids = append(ids, model.ID)
	}
	if strings.Join(ids, ",") != "cloudflare/clef,cloudflare/clef-flash" {
		t.Fatalf("catalog = %v", ids)
	}

	// The Typesafe flavor keeps its own identity, path and catalog.
	jev, err := NewTypesafeProvider(&schemas.ProviderConfig{}, noopLogger{})
	if err != nil || jev.GetProviderKey() != schemas.Typesafe {
		t.Fatalf("typesafe identity changed: %v", err)
	}
}
