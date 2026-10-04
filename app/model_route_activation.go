package app

import (
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/modelconfig"
	"github.com/levmv/skot/model"
)

func savedContextWindowFromState(state *agent.State) *modelconfig.SavedContextWindow {
	if state == nil || state.Configured == nil || state.Configured.RuntimePolicy.ContextWindow <= 0 ||
		strings.TrimSpace(state.Selection.Provider) == "" || strings.TrimSpace(state.Selection.Model) == "" {
		return nil
	}
	return &modelconfig.SavedContextWindow{
		URI:       strings.ToLower(strings.TrimSpace(state.Selection.Provider)) + "/" + strings.TrimSpace(state.Selection.Model),
		Endpoint:  strings.TrimSpace(state.Configured.Environment.Endpoint),
		Window:    state.Configured.RuntimePolicy.ContextWindow,
		Estimated: state.Configured.RuntimePolicy.ContextWindowEstimated,
	}
}

func savedContextWindowFromInfo(info model.Info) *modelconfig.SavedContextWindow {
	if info.ContextWindow <= 0 || strings.TrimSpace(info.Provider) == "" || strings.TrimSpace(info.Model) == "" {
		return nil
	}
	return &modelconfig.SavedContextWindow{
		URI:       strings.ToLower(strings.TrimSpace(info.Provider)) + "/" + strings.TrimSpace(info.Model),
		Endpoint:  strings.TrimSpace(info.Endpoint),
		Window:    info.ContextWindow,
		Estimated: info.ContextWindowEstimated,
	}
}
