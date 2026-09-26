package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
)

const headroomRetrieveTool = "headroom_retrieve"

var headroomHashPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// HeadroomMCPHandler serves the single-purpose, stateless Headroom MCP endpoint.
// Protocol handling, authentication, and governance admission are delegated to
// MCPServerHandler; only the fixed tool implementation lives here.
type HeadroomMCPHandler struct {
	mcp *MCPServerHandler
}

func NewHeadroomMCPHandler(ctx context.Context, config *lib.Config, admitter MCPGatewayAdmitter, identityResolver OAuth2IdentityResolver, vkCache VirtualKeyCache) (*HeadroomMCPHandler, error) {
	base := NewHeadroomHandler()
	if len(base.token) < 32 || base.client == nil || base.endpoint == "" {
		return nil, nil
	}
	manager := &headroomToolManager{
		token:    base.token,
		endpoint: strings.TrimSuffix(base.endpoint, "/v1/events") + "/v1/retrieve",
		client:   newHeadroomHTTPClient(2 * time.Second),
	}
	mcpHandler, err := NewMCPServerHandler(ctx, config, manager, admitter, identityResolver, vkCache)
	if err != nil {
		return nil, err
	}
	return &HeadroomMCPHandler{mcp: mcpHandler}, nil
}

// RegisterRoutes exposes POST only: no SSE and no general-purpose /mcp route.
func (h *HeadroomMCPHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	if h != nil && h.mcp != nil {
		r.POST("/v1/headroom/mcp", lib.ChainMiddlewares(h.mcp.handleMCPServer, middlewares...))
	}
}

type headroomToolManager struct {
	token    string
	endpoint string
	client   *http.Client
}

func (m *headroomToolManager) GetAvailableMCPTools(context.Context) []schemas.ChatTool {
	readOnly, destructive, idempotent, openWorld := true, false, true, false
	description := "Retrieve the exact original content using an opaque Headroom handle."
	additionalProperties := schemas.AdditionalPropertiesStruct{AdditionalPropertiesBool: &destructive}
	return []schemas.ChatTool{{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name:        headroomRetrieveTool,
			Description: &description,
			Parameters: &schemas.ToolFunctionParameters{
				Type: "object",
				Properties: schemas.NewOrderedMapFromPairs(schemas.KV("hash", map[string]any{
					"type": "string", "pattern": "^[0-9a-fA-F]{64}$",
				})),
				Required:             []string{"hash"},
				AdditionalProperties: &additionalProperties,
			},
		},
		Annotations: &schemas.MCPToolAnnotations{ReadOnlyHint: &readOnly, DestructiveHint: &destructive, IdempotentHint: &idempotent, OpenWorldHint: &openWorld},
	}}
}

func (m *headroomToolManager) ExecuteChatMCPTool(ctx context.Context, call *schemas.ChatAssistantMessageToolCall) (*schemas.ChatMessage, *schemas.BifrostError) {
	if ctx == nil || call == nil || call.Function.Name == nil || *call.Function.Name != headroomRetrieveTool {
		return nil, headroomToolError("Headroom retrieval unavailable")
	}
	// mcp-go wraps the admitted context. Bifrost's constructor preserves the
	// nearest grant through that wrapper instead of relying on its concrete type.
	bfCtx := schemas.NewBifrostContext(ctx, schemas.NoDeadline)
	grant := bfCtx.Grant()
	if grant == nil || grant.Access() == nil || grant.Limits() == nil || grant.Identity() == nil {
		return nil, headroomToolError("Headroom retrieval unavailable")
	}
	identity := grant.Identity()
	principal := ""
	if vk := identity.VirtualKey(); vk != nil && vk.ID != "" {
		principal = "vk:" + vk.ID
	} else if user := identity.User(); user != nil && user.ID != "" {
		principal = "user:" + user.ID
	}
	if principal == "" {
		return nil, headroomToolError("Headroom retrieval unavailable")
	}
	project := ""
	if ref := identity.Project(); ref != nil {
		project = ref.ID
	}
	var args struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || !headroomHashPattern.MatchString(args.Hash) {
		return nil, headroomToolError("Invalid Headroom retrieval request")
	}
	body, err := json.Marshal(struct {
		Handle    string `json:"handle"`
		Principal string `json:"principal"`
		Project   string `json:"project"`
	}{Handle: strings.ToLower(args.Hash), Principal: principal, Project: project})
	if err != nil {
		return nil, headroomToolError("Headroom retrieval unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, headroomToolError("Headroom retrieval unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, headroomToolError("Headroom retrieval unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, headroomToolError("Headroom retrieval unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if err != nil || len(data) > 32<<20 {
		return nil, headroomToolError("Headroom retrieval unavailable")
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, headroomToolError("Headroom retrieval unavailable")
	}
	return &schemas.ChatMessage{Role: schemas.ChatMessageRoleTool, Content: &schemas.ChatMessageContent{ContentStr: &result.Content}}, nil
}

func (m *headroomToolManager) ExecuteResponsesMCPTool(context.Context, *schemas.ResponsesToolMessage) (*schemas.ResponsesMessage, *schemas.BifrostError) {
	return nil, headroomToolError("Headroom retrieval unavailable")
}

func headroomToolError(message string) *schemas.BifrostError {
	status := http.StatusServiceUnavailable
	return &schemas.BifrostError{IsBifrostError: true, StatusCode: &status, Error: &schemas.ErrorField{Message: message}}
}

var _ MCPToolManager = (*headroomToolManager)(nil)
