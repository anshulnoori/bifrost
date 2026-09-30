package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	grantpkg "github.com/maximhq/bifrost/framework/grant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeadroomToolAdvertisesOnlyReadOnlyRetrieve(t *testing.T) {
	tools := (&headroomToolManager{}).GetAvailableMCPTools(context.Background())
	require.Len(t, tools, 1)
	assert.Equal(t, headroomRetrieveTool, tools[0].Function.Name)
	assert.Equal(t, []string{"hash"}, tools[0].Function.Parameters.Required)
	require.NotNil(t, tools[0].Annotations.ReadOnlyHint)
	assert.True(t, *tools[0].Annotations.ReadOnlyHint)
}

func TestHeadroomToolUsesGrantOwnerAndReturnsContent(t *testing.T) {
	hash := strings.Repeat("a", 64)
	var received map[string]string
	plugin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer "+strings.Repeat("t", 32), r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"retrieved text"}`))
	}))
	defer plugin.Close()

	manager := &headroomToolManager{token: strings.Repeat("t", 32), endpoint: plugin.URL, client: newHeadroomHTTPClient(time.Second)}
	ctx := headroomGrantedContext(t, context.Background())
	name := headroomRetrieveTool
	message, toolErr := manager.ExecuteChatMCPTool(context.WithValue(ctx, struct{}{}, "mcp wrapper"), &schemas.ChatAssistantMessageToolCall{Function: schemas.ChatAssistantMessageToolCallFunction{
		Name: &name, Arguments: `{"hash":"` + hash + `","principal":"user:attacker","project":"attacker"}`,
	}})
	require.Nil(t, toolErr)
	require.NotNil(t, message)
	assert.Equal(t, "retrieved text", *message.Content.ContentStr)
	assert.Equal(t, map[string]string{"handle": hash, "principal": "vk:vk-1", "project": "project-1"}, received)
}

func TestHeadroomToolAcceptsVirtualKeyWithoutProject(t *testing.T) {
	plugin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		assert.Equal(t, "vk:vk-1", received["principal"])
		assert.Empty(t, received["project"])
		_, _ = w.Write([]byte(`{"content":"original without a project"}`))
	}))
	defer plugin.Close()
	manager := &headroomToolManager{endpoint: plugin.URL, client: newHeadroomHTTPClient(time.Second)}
	ctx := headroomGrantedContextWithProject(t, context.Background(), nil)
	name := headroomRetrieveTool
	message, toolErr := manager.ExecuteChatMCPTool(ctx, &schemas.ChatAssistantMessageToolCall{Function: schemas.ChatAssistantMessageToolCallFunction{
		Name: &name, Arguments: `{"hash":"` + strings.Repeat("a", 64) + `"}`,
	}})
	require.Nil(t, toolErr)
	require.NotNil(t, message)
	assert.Equal(t, "original without a project", *message.Content.ContentStr)
}

func TestHeadroomToolWrongOwnerIsOpaque(t *testing.T) {
	plugin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }))
	defer plugin.Close()
	manager := &headroomToolManager{token: strings.Repeat("t", 32), endpoint: plugin.URL, client: newHeadroomHTTPClient(time.Second)}
	name := headroomRetrieveTool
	_, toolErr := manager.ExecuteChatMCPTool(headroomGrantedContext(t, context.Background()), &schemas.ChatAssistantMessageToolCall{Function: schemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: `{"hash":"` + strings.Repeat("b", 64) + `"}`}})
	require.NotNil(t, toolErr)
	assert.Equal(t, "Headroom retrieval unavailable", toolErr.Error.Message)
	assert.NotContains(t, toolErr.Error.Message, "owner")
}

func TestHeadroomToolRequiresCompleteAuthenticatedGrant(t *testing.T) {
	manager := &headroomToolManager{}
	name := headroomRetrieveTool
	call := &schemas.ChatAssistantMessageToolCall{Function: schemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: `{"hash":"` + strings.Repeat("c", 64) + `"}`}}
	ctx := schemas.NewBifrostContext(context.Background(), time.Time{})
	_, toolErr := manager.ExecuteChatMCPTool(ctx, call)
	require.NotNil(t, toolErr)
}

func TestHeadroomToolPropagatesCancellation(t *testing.T) {
	started := make(chan struct{})
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	manager := &headroomToolManager{token: strings.Repeat("t", 32), endpoint: "http://127.0.0.1:1/v1/retrieve", client: client}
	parent, cancel := context.WithCancel(context.Background())
	ctx := headroomGrantedContext(t, parent)
	name := headroomRetrieveTool
	done := make(chan *schemas.BifrostError, 1)
	go func() {
		_, err := manager.ExecuteChatMCPTool(ctx, &schemas.ChatAssistantMessageToolCall{Function: schemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: `{"hash":"` + strings.Repeat("d", 64) + `"}`}})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		require.NotNil(t, err)
	case <-time.After(time.Second):
		t.Fatal("retrieval did not stop after cancellation")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func headroomGrantedContext(t *testing.T, parent context.Context) *schemas.BifrostContext {
	t.Helper()
	return headroomGrantedContextWithProject(t, parent, &schemas.EntityRef{ID: "project-1"})
}

func headroomGrantedContextWithProject(t *testing.T, parent context.Context, project *schemas.EntityRef) *schemas.BifrostContext {
	t.Helper()
	ctx := schemas.NewBifrostContext(parent, time.Time{})
	g := grantpkg.New()
	require.True(t, g.SetIdentity(grantpkg.NewIdentity(
		grantpkg.NewCredential(grantpkg.CredentialVirtualKey, "presented"),
		&schemas.UserRef{ID: "user-1"}, &schemas.EntityRef{ID: "vk-1"}, nil, nil, nil, project,
	)))
	require.True(t, g.SetAccess(grantpkg.NewAccess(nil, nil, grantpkg.Intersect, nil)))
	require.True(t, g.SetLimits(grantpkg.NewLimits(nil, nil)))
	require.True(t, ctx.SetGrant(g))
	return ctx
}
