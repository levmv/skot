package modelconfig

import (
	"fmt"

	"github.com/levmv/skot/model"
	"github.com/levmv/skot/model/anthropic"
	"github.com/levmv/skot/model/chatcompletions"
	"github.com/levmv/skot/model/google"
	"github.com/levmv/skot/model/responses"
)

// SavedItemProjection restores only the journal's replay policy. Current model
// declarations, endpoints, credentials and reasoning settings are irrelevant
// to this read. A nil projection preserves all provider-owned items.
func SavedItemProjection(selection model.ReplayContext) (func([]model.Item) []model.Item, error) {
	contract := selection.ProviderStateContract
	switch APIFromBackendID(selection.Backend) {
	case ChatCompletions:
		for _, policy := range []chatcompletions.ReasoningReplayPolicy{
			"", chatcompletions.ReasoningReplayCurrentTurn, chatcompletions.ReasoningReplayAllTurns,
		} {
			if (chatcompletions.RouteTraits{ReasoningReplay: policy}).ProviderStateContract() == contract {
				return policy.ProjectModelItems, nil
			}
		}
	case Responses:
		if contract == (responses.RouteTraits{}).ProviderStateContract() {
			return nil, nil
		}
	case AnthropicMessages:
		if contract == anthropic.ProviderStateContract {
			return nil, nil
		}
	case Gemini:
		if contract == google.ProviderStateContract {
			return nil, nil
		}
	default:
		return nil, fmt.Errorf("unknown saved model backend %q", selection.Backend)
	}
	return nil, fmt.Errorf("unsupported saved replay contract %q for backend %q", contract, selection.Backend)
}
