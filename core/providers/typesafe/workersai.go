package typesafe

import (
	"errors"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	schemas "github.com/maximhq/bifrost/core/schemas"
)

// Cloudflare's Clef decision models speak the System One API on Workers AI:
// POST <base>/@cf/cloudflare/<model> with the same state/questions body and the
// same answers, wrapped in Cloudflare's {"result": ..., "success": ...} envelope.
// The base URL names the account, so it has no default:
//
//	https://api.cloudflare.com/client/v4/accounts/<account_id>/ai/run
//	https://gateway.ai.cloudflare.com/v1/<account_id>/<gateway>/workers-ai
const workersAIModelPrefix = "/@cf/cloudflare/"

// workersAIModelName bounds the model segment of the upstream path.
var workersAIModelName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// cloudflareModels is the System One catalog Cloudflare hosts on Workers AI.
var cloudflareModels = []typesafeModel{
	{ID: "clef", Name: "Clef", Description: "Cloudflare's 27B multimodal decision model (System One API)", ReleaseDate: "2026-10-01T00:00:00+00:00"},
	{ID: "clef-flash", Name: "Clef Flash", Description: "Cloudflare's 9B low-latency decision model (System One API)", ReleaseDate: "2026-10-01T00:00:00+00:00"},
}

// NewCloudflareProvider serves Clef decisions from a Workers AI account. The key
// value is a Cloudflare API token with Workers AI permission.
func NewCloudflareProvider(config *schemas.ProviderConfig, logger schemas.Logger) (*TypesafeProvider, error) {
	if strings.TrimSpace(config.NetworkConfig.BaseURL) == "" {
		return nil, errors.New("cloudflare requires network_config.base_url, e.g. https://api.cloudflare.com/client/v4/accounts/<account_id>/ai/run")
	}
	provider, err := NewTypesafeProvider(config, logger)
	if err != nil {
		return nil, err
	}
	provider.providerKey = schemas.Cloudflare
	provider.workersAI = true
	provider.models = cloudflareModels
	return provider, nil
}

// workersAIURL builds the per-model run URL. The model is a path segment, so it
// is restricted to a plain model name.
func (provider *TypesafeProvider) workersAIURL(model string) (string, *schemas.BifrostError) {
	if !workersAIModelName.MatchString(model) {
		return "", providerUtils.NewBifrostBadRequestError("cloudflare decision model must be a plain model name such as clef or clef-flash")
	}
	return provider.networkConfig.BaseURL + workersAIModelPrefix + model, nil
}

// unwrapWorkersAI returns the System One body inside Cloudflare's REST envelope.
// Bodies without the envelope (a binding-style proxy) pass through unchanged.
func unwrapWorkersAI(body []byte) []byte {
	if result := gjson.GetBytes(body, "result"); result.IsObject() && gjson.GetBytes(body, "success").Exists() {
		return []byte(result.Raw)
	}
	return body
}
