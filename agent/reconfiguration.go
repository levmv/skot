package agent

import (
	"context"
	"fmt"
)

// runtimeConfiguration stays fixed through a model response and its tool calls.
// A pending copy takes effect at the next request boundary. Scope remains live
// and is read when applying the selection.
type runtimeConfiguration struct {
	backend      Backend
	modelInfo    ModelInfo
	tools        []Tool
	toolByName   map[string]Tool
	toolSet      string
	programTools []ProgramToolSnapshot
}

func (runtime *Runtime) selectedConfigurationLocked() runtimeConfiguration {
	if runtime.pendingConfig != nil {
		return *runtime.pendingConfig
	}
	return runtime.runtimeConfiguration
}

func (runtime *Runtime) reconfigure(ctx context.Context, change func(*runtimeConfiguration)) error {
	runtime.configMu.Lock()
	defer runtime.configMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Run takes configMu when starting and finishing a turn. Choosing whether
	// to apply or defer this selection is therefore synchronized with both.
	// TryLock avoids waiting for runMu while holding configMu.
	idle := runtime.runMu.TryLock()
	if idle {
		defer runtime.runMu.Unlock()
	} else if !runtime.turnActive {
		return ErrRunActive
	}
	configuration := runtime.selectedConfigurationLocked()
	change(&configuration)
	snapshot := runtime.effectiveConfigSnapshotWithProgramToolsLocked(configuration.modelInfo,
		configuration.tools, configuration.toolSet, runtime.scope, configuration.programTools)
	if err := validateEffectiveConfigSnapshot(snapshot); err != nil {
		return err
	}
	if !idle {
		runtime.pendingConfig = &configuration
		return nil
	}
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return fmt.Errorf("read journal before reconfiguration: %w", err)
	}
	live, err := reduceRecords(records)
	if err != nil {
		return err
	}
	if live.state.hasUnfinishedWork() {
		return unfinishedWorkError("changing configuration")
	}
	return runtime.applyConfigurationLocked(ctx, live, configuration)
}

func (runtime *Runtime) applyPendingConfiguration(ctx context.Context, live *stateReducer) error {
	runtime.configMu.Lock()
	defer runtime.configMu.Unlock()
	return runtime.applyPendingConfigurationLocked(ctx, live)
}

func (runtime *Runtime) applyPendingConfigurationLocked(ctx context.Context, live *stateReducer) error {
	if runtime.pendingConfig == nil {
		return nil
	}
	return runtime.applyConfigurationLocked(ctx, live, *runtime.pendingConfig)
}

func (runtime *Runtime) applyConfigurationLocked(ctx context.Context, live *stateReducer, configuration runtimeConfiguration) error {
	modelInfo := configuration.modelInfo
	if live.state.SessionID != "" && !selectionMatchesModel(live.state.Selection, modelInfo) {
		epoch, err := newID("epoch")
		if err != nil {
			return err
		}
		selection := ModelSelectedRecord{
			Backend:               modelInfo.BackendID,
			Provider:              modelInfo.Provider,
			Model:                 modelInfo.Model,
			ReasoningEffort:       modelInfo.ReasoningEffort,
			ProviderStateContract: modelInfo.ProviderStateContract,
			Epoch:                 epoch,
		}
		if _, err := appendRecordAndApply(ctx, runtime.journal, live, RecordModelSelected, selection); err != nil {
			return err
		}
	}
	snapshot := runtime.effectiveConfigSnapshotWithProgramToolsLocked(modelInfo, configuration.tools,
		configuration.toolSet, runtime.scope, configuration.programTools)
	if err := runtime.recordEffectiveConfigurationAndApply(ctx, live, snapshot); err != nil {
		return err
	}
	runtime.runtimeConfiguration = configuration
	runtime.pendingConfig = nil
	runtime.publishSessionStatus(live.state)
	return nil
}
