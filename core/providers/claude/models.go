package claude

import (
	"strings"
	"time"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

// claudeModel is one entry of the subscription catalog.
type claudeModel struct {
	ID            string
	Name          string
	ContextLength int
}

// claudeModels lists the models verified to serve through the subscription
// bridge, newest family first. The bridge exposes only native Messages, so there
// is no upstream listing endpoint; update this list when the subscription
// gains or retires a model. Pricing is backfilled from the datasheet at Anthropic
// reference rates, never hardcoded here.
var claudeModels = []claudeModel{
	{ID: "claude-fable-5-1", Name: "Claude Fable 5.1", ContextLength: 1_000_000},
	{ID: "claude-opus-5-5", Name: "Claude Opus 5.5", ContextLength: 1_000_000},
	{ID: "claude-opus-5", Name: "Claude Opus 5", ContextLength: 1_000_000},
	{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5", ContextLength: 1_000_000},
	{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", ContextLength: 1_000_000},
	{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5", ContextLength: 200_000},
}

// ListModels serves the static subscription catalog through the standard
// pipeline, so account-key allow lists, block lists and aliases apply. It makes
// no bridge or upstream call.
func (p *ClaudeProvider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	startTime := time.Now()
	response, err := providerUtils.HandleMultipleListModelsRequests(ctx, keys, request, p.listModelsByKey)
	if err != nil {
		return nil, err
	}
	response.ExtraFields.Latency = time.Since(startTime).Milliseconds()
	return response, nil
}

func (p *ClaudeProvider) listModelsByKey(_ *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	response := &schemas.BifrostListModelsResponse{Data: make([]schemas.Model, 0, len(claudeModels))}
	pipeline := &providerUtils.ListModelsPipeline{
		AllowedModels:     key.Models,
		BlacklistedModels: key.BlacklistedModels,
		Aliases:           key.Aliases,
		Unfiltered:        request.Unfiltered,
		ProviderKey:       schemas.Claude,
		MatchFns:          providerUtils.DefaultMatchFns(),
	}
	if pipeline.ShouldEarlyExit() {
		return response, nil
	}
	included := make(map[string]bool)
	for _, model := range claudeModels {
		for _, result := range pipeline.FilterModel(model.ID) {
			entry := schemas.Model{
				ID:            string(schemas.Claude) + "/" + result.ResolvedID,
				Name:          schemas.Ptr(model.Name),
				ContextLength: schemas.Ptr(model.ContextLength),
				OwnedBy:       schemas.Ptr("anthropic"),
			}
			if result.AliasValue != "" {
				entry.Alias = schemas.Ptr(result.AliasValue)
			}
			response.Data = append(response.Data, entry)
			included[strings.ToLower(result.ResolvedID)] = true
		}
	}
	response.Data = append(response.Data, pipeline.BackfillModels(included)...)
	return response, nil
}
