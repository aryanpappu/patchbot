package openai

import (
	"context"
	"net/http"

	"github.com/aryanpappu/patchbot/internal/llm"
)

type modelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int    `json:"context_length"`
		ContextWindow int    `json:"context_window"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

// ListModels fetches /models. It accepts the optional has_more/last_id cursor
// extension using ?after=, and bounds pagination to 100 pages. All pages use
// the configured endpoint; provider-supplied links are never followed.
// Fallback, merging and caching belong to llm.ModelLister.
func (p *Provider) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, requestTimeout, errRequestTimeout)
	defer cancel()
	target := p.endpoint("models")
	var models []llm.ModelInfo
	seen, cursors := map[string]bool{}, map[string]bool{}
	for range 100 {
		if err := p.contextFailure(ctx); err != nil {
			return nil, err
		}
		var page modelsResponse
		if _, err := p.request(ctx, http.MethodGet, target, nil, &page); err != nil {
			return nil, err
		}
		if page.Data == nil {
			return nil, p.failure(llm.ErrMalformed, "model response has no data list", nil)
		}
		for _, model := range page.Data {
			if model.ID == "" || seen[model.ID] {
				continue
			}
			seen[model.ID] = true
			window := model.ContextLength
			if window == 0 {
				window = model.ContextWindow
			}
			if window < 0 {
				return nil, p.failure(llm.ErrMalformed, "negative model context window", nil)
			}
			models = append(models, llm.ModelInfo{ID: model.ID, DisplayName: model.Name, ContextWindow: window, Source: llm.SourceLive})
		}
		if !page.HasMore {
			return models, nil
		}
		if page.LastID == "" || cursors[page.LastID] || len(page.Data) == 0 {
			return nil, p.failure(llm.ErrMalformed, "invalid model pagination cursor", nil)
		}
		cursors[page.LastID] = true
		query := target.Query()
		query.Set("after", page.LastID)
		target.RawQuery = query.Encode()
	}
	return nil, p.failure(llm.ErrMalformed, "model pagination exceeds 100 pages", nil)
}
