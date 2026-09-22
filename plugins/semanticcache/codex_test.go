package semanticcache

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestCodexNeverUsesSharedCache(t *testing.T) {
	// Deliberately no cache client/config: neither path may consult one even
	// when a caller explicitly requests the shared cache bucket.
	plugin := &Plugin{}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(CacheKey, "shared")
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "codex-isolation")
	req := &schemas.BifrostRequest{RequestType: schemas.ResponsesRequest, ResponsesRequest: &schemas.BifrostResponsesRequest{Provider: schemas.Codex, Model: "gpt-5.3-codex"}}
	got, short, err := plugin.PreLLMHook(ctx, req)
	if got != req || short != nil || err != nil {
		t.Fatalf("Codex cache lookup was not bypassed: %v %v", short, err)
	}
	res := &schemas.BifrostResponse{ResponsesResponse: &schemas.BifrostResponsesResponse{ExtraFields: schemas.BifrostResponseExtraFields{Provider: schemas.Codex, RequestType: schemas.ResponsesRequest}}}
	response, bfErr, err := plugin.PostLLMHook(ctx, res, nil)
	if response != res || bfErr != nil || err != nil {
		t.Fatalf("Codex cache write was not bypassed: %v %v", bfErr, err)
	}
}
