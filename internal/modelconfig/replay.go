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
// to this read. An unset projection function preserves all provider-owned items.
func SavedItemProjection(selection model.ReplayContext) (model.ItemProjection, error) {
	contract := selection.ProviderStateContract
	switch APIFromBackendID(selection.Backend) {
	case ChatCompletions:
		for _, policy := range []chatcompletions.ReasoningReplayPolicy{
			"", chatcompletions.ReasoningReplayCurrentTurn, chatcompletions.ReasoningReplayAllTurns,
		} {
			if (chatcompletions.RouteTraits{ReasoningReplay: policy}).ProviderStateContract() == contract {
				return model.ItemProjection{ProjectModelItems: policy.ProjectModelItems}, nil
			}
		}
	case Responses:
		if contract == (responses.RouteTraits{}).ProviderStateContract() {
			return model.ItemProjection{ProjectModelItems: responses.ProjectModelItems, OpaqueReasoningBytes: responses.OpaqueReasoningBytes}, nil
		}
	case AnthropicMessages:
		if contract == anthropic.ProviderStateContract {
			return model.ItemProjection{ProjectModelItems: anthropic.ProjectModelItems, OpaqueReasoningBytes: anthropic.OpaqueReasoningBytes}, nil
		}
	case Gemini:
		if contract == google.ProviderStateContract {
			return model.ItemProjection{ProjectModelItems: google.ProjectModelItems, OpaqueReasoningBytes: google.OpaqueReasoningBytes}, nil
		}
	default:
		return model.ItemProjection{}, fmt.Errorf("unknown saved model backend %q", selection.Backend)
	}
	return model.ItemProjection{}, fmt.Errorf("unsupported saved replay contract %q for backend %q", contract, selection.Backend)
}
