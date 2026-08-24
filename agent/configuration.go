package agent

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

func (runtime *Runtime) effectiveConfigSnapshotLocked(modelInfo ModelInfo, tools []Tool, toolSet string, scope ScopeSnapshot) EffectiveConfigSnapshot {
	return runtime.effectiveConfigSnapshotWithProgramToolsLocked(modelInfo, tools, toolSet, scope, runtime.programTools)
}

func (runtime *Runtime) effectiveConfigSnapshotWithProgramToolsLocked(modelInfo ModelInfo, tools []Tool, toolSet string, scope ScopeSnapshot, programTools []ProgramToolSnapshot) EffectiveConfigSnapshot {
	toolSpecs := make([]ToolSpec, 0, len(tools))
	for _, tool := range tools {
		toolSpecs = append(toolSpecs, cloneToolSpec(tool.Spec))
	}
	return EffectiveConfigSnapshot{
		ModelContext: ModelContextSnapshot{
			Instructions:           runtime.instructions,
			CompactionInstructions: compactionInstructions,
			ToolLimitInstructions:  toolLimitInstructions,
			ToolSet:                strings.TrimSpace(toolSet),
			Tools:                  toolSpecs,
		},
		RuntimePolicy: RuntimePolicySnapshot{
			ContextWindow:          modelInfo.ContextWindow,
			ContextWindowEstimated: modelInfo.ContextWindowEstimated,
			ImageInputUnsupported:  modelInfo.ImageInputUnsupported,
			MaxModelAttempts:       runtime.requestPolicy.MaxAttempts,
			RetryBudget:            durationSnapshot(runtime.requestPolicy.RetryBudget),
			RetryBaseDelay:         durationSnapshot(runtime.requestPolicy.BaseDelay),
			RetryMaxDelay:          durationSnapshot(runtime.requestPolicy.MaxDelay),
			StreamIdleTimeout:      durationSnapshot(runtime.requestPolicy.StreamIdleTimeout),
			MaxToolIterations:      runtime.maxToolIterations,
			MaxRequestBytes:        modelInfo.MaxRequestBytes,
			MaxCompletionBytes:     modelInfo.MaxCompletionBytes,
			AwaitRequiredJobs:      runtime.awaitRequiredJobs,
		},
		Environment: ExecutionEnvironmentSnapshot{
			Endpoint:     runtime.sanitize(strings.TrimSpace(modelInfo.Endpoint)),
			Build:        runtime.build,
			Scope:        scope,
			ProgramTools: activeProgramToolSnapshots(programTools, tools),
		},
	}
}

func activeProgramToolSnapshots(configured []ProgramToolSnapshot, tools []Tool) []ProgramToolSnapshot {
	if len(configured) == 0 || len(tools) == 0 {
		return nil
	}
	active := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		active[tool.Spec.Name] = struct{}{}
	}
	result := make([]ProgramToolSnapshot, 0, len(configured))
	for _, snapshot := range configured {
		if _, exists := active[snapshot.Name]; !exists {
			continue
		}
		copy := snapshot
		copy.Command = append([]string(nil), snapshot.Command...)
		copy.EnvironmentNames = append([]string(nil), snapshot.EnvironmentNames...)
		result = append(result, copy)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (runtime *Runtime) recordEffectiveConfigurationAndApply(ctx context.Context, reducer *stateReducer, snapshot EffectiveConfigSnapshot) error {
	state := &reducer.state
	if state.SessionID == "" {
		return nil
	}
	if state.Selection.Epoch == "" {
		return errors.New("cannot configure a session before selecting a model")
	}
	if err := validateEffectiveConfigSnapshot(snapshot); err != nil {
		return err
	}
	if state.Configured != nil && equalEffectiveConfigSnapshots(*state.Configured, snapshot) {
		return nil
	}
	_, err := appendRecordAndApply(ctx, runtime.journal, reducer, RecordSessionConfigured, snapshot)
	return err
}

func (runtime *Runtime) recordCurrentEffectiveConfigurationAndApply(ctx context.Context, reducer *stateReducer) error {
	runtime.configMu.RLock()
	defer runtime.configMu.RUnlock()
	snapshot := runtime.effectiveConfigSnapshotLocked(runtime.modelInfo, runtime.tools, runtime.toolSet, runtime.scope)
	return runtime.recordEffectiveConfigurationAndApply(ctx, reducer, snapshot)
}

func equalEffectiveConfigSnapshots(left, right EffectiveConfigSnapshot) bool {
	// Compare the journal encoding so formatting-only differences in RawMessage
	// tool schemas do not create new session configuration records.
	leftJSON, leftErr := json.Marshal(left, json.Deterministic(true))
	rightJSON, rightErr := json.Marshal(right, json.Deterministic(true))
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func validateEffectiveConfigSnapshot(snapshot EffectiveConfigSnapshot) error {
	if strings.TrimSpace(snapshot.ModelContext.CompactionInstructions) == "" {
		return errors.New("configured compaction instructions are required")
	}
	if strings.TrimSpace(snapshot.ModelContext.ToolLimitInstructions) == "" {
		return errors.New("configured tool limit instructions are required")
	}
	if snapshot.RuntimePolicy.ContextWindow < 0 {
		return errors.New("configured context window cannot be negative")
	}
	if snapshot.RuntimePolicy.ContextWindowEstimated && snapshot.RuntimePolicy.ContextWindow == 0 {
		return errors.New("configured estimated context window must be positive")
	}
	if snapshot.RuntimePolicy.MaxModelAttempts < -1 || snapshot.RuntimePolicy.MaxModelAttempts == 0 {
		return errors.New("configured max model attempts must be positive or -1")
	}
	for name, value := range map[string]string{
		"retry budget":        snapshot.RuntimePolicy.RetryBudget,
		"retry base delay":    snapshot.RuntimePolicy.RetryBaseDelay,
		"retry max delay":     snapshot.RuntimePolicy.RetryMaxDelay,
		"stream idle timeout": snapshot.RuntimePolicy.StreamIdleTimeout,
	} {
		if value == "" {
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return fmt.Errorf("configured %s must be a positive duration", name)
		}
	}
	if snapshot.RuntimePolicy.MaxModelAttempts == -1 && snapshot.RuntimePolicy.RetryBudget == "" {
		return errors.New("configured unlimited model attempts require a retry budget")
	}
	if snapshot.RuntimePolicy.MaxModelAttempts == -1 && snapshot.RuntimePolicy.RetryBaseDelay == "" {
		return errors.New("configured unlimited model attempts require a retry base delay")
	}
	if snapshot.RuntimePolicy.MaxToolIterations < -1 || snapshot.RuntimePolicy.MaxToolIterations == 0 {
		return errors.New("configured max tool iterations must be positive or -1")
	}
	if snapshot.RuntimePolicy.MaxRequestBytes < 0 || snapshot.RuntimePolicy.MaxCompletionBytes < 0 {
		return errors.New("configured model byte limits cannot be negative")
	}
	tools, err := NormalizeToolSpecs(snapshot.ModelContext.Tools)
	if err != nil {
		return fmt.Errorf("configured model tools: %w", err)
	}
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		seen[tool.Name] = struct{}{}
	}
	seenPrograms := make(map[string]struct{}, len(snapshot.Environment.ProgramTools))
	for _, tool := range snapshot.Environment.ProgramTools {
		if err := validateProgramToolSnapshot(tool); err != nil {
			return err
		}
		name := strings.TrimSpace(tool.Name)
		if _, exists := seenPrograms[name]; exists {
			return fmt.Errorf("duplicate configured program tool %q", name)
		}
		seenPrograms[name] = struct{}{}
		if _, exists := seen[name]; !exists {
			return fmt.Errorf("configured program tool %q is absent from the model tool catalog", name)
		}
	}
	return nil
}

func validateProgramToolSnapshot(tool ProgramToolSnapshot) error {
	name := strings.TrimSpace(tool.Name)
	if name == "" || strings.TrimSpace(tool.Program) == "" || len(tool.Command) == 0 {
		return errors.New("configured program tool requires name, program, and command")
	}
	if duration, err := time.ParseDuration(tool.Timeout); err != nil || duration <= 0 {
		return fmt.Errorf("configured program tool %q has invalid timeout", name)
	}
	if tool.Yield != "" {
		if duration, err := time.ParseDuration(tool.Yield); err != nil || duration <= 0 {
			return fmt.Errorf("configured program tool %q has invalid yield", name)
		}
	}
	switch tool.Background {
	case "never", "auto", "always":
	default:
		return fmt.Errorf("configured program tool %q has invalid background mode %q", name, tool.Background)
	}
	if tool.Detach && tool.Background == "never" && tool.Yield == "" {
		return fmt.Errorf("configured program tool %q detaches work that cannot outlive its call", name)
	}
	return nil
}

func durationSnapshot(value time.Duration) string {
	if value <= 0 {
		return ""
	}
	return value.String()
}

func cloneEffectiveConfigSnapshot(snapshot EffectiveConfigSnapshot) EffectiveConfigSnapshot {
	cloned := snapshot
	if snapshot.Environment.Build.Modified != nil {
		modified := *snapshot.Environment.Build.Modified
		cloned.Environment.Build.Modified = &modified
	}
	cloned.ModelContext.Tools = make([]ToolSpec, len(snapshot.ModelContext.Tools))
	for index, tool := range snapshot.ModelContext.Tools {
		cloned.ModelContext.Tools[index] = cloneToolSpec(tool)
	}
	cloned.Environment.Scope.ProtectedPaths = append([]string(nil), snapshot.Environment.Scope.ProtectedPaths...)
	cloned.Environment.Scope.AddedPaths = append([]string(nil), snapshot.Environment.Scope.AddedPaths...)
	cloned.Environment.ProgramTools = make([]ProgramToolSnapshot, len(snapshot.Environment.ProgramTools))
	for index, tool := range snapshot.Environment.ProgramTools {
		cloned.Environment.ProgramTools[index] = tool
		cloned.Environment.ProgramTools[index].Command = append([]string(nil), tool.Command...)
		cloned.Environment.ProgramTools[index].EnvironmentNames = append([]string(nil), tool.EnvironmentNames...)
	}
	return cloned
}

func cloneToolSpec(spec ToolSpec) ToolSpec {
	spec.InputSchema = spec.InputSchema.Clone()
	return spec
}

func cloneToolSpecs(specs []ToolSpec) []ToolSpec {
	cloned := make([]ToolSpec, len(specs))
	for index, spec := range specs {
		cloned[index] = cloneToolSpec(spec)
	}
	return cloned
}

func sanitizeScopeSnapshot(snapshot ScopeSnapshot, sanitize func(string) string) ScopeSnapshot {
	snapshot.Scope = sanitize(strings.TrimSpace(snapshot.Scope))
	snapshot.AddedPaths = append([]string(nil), snapshot.AddedPaths...)
	for index := range snapshot.AddedPaths {
		snapshot.AddedPaths[index] = sanitize(strings.TrimSpace(snapshot.AddedPaths[index]))
	}
	snapshot.ProtectedPaths = append([]string(nil), snapshot.ProtectedPaths...)
	for index := range snapshot.ProtectedPaths {
		snapshot.ProtectedPaths[index] = sanitize(strings.TrimSpace(snapshot.ProtectedPaths[index]))
	}
	snapshot.Backend = sanitize(strings.TrimSpace(snapshot.Backend))
	snapshot.Network = sanitize(strings.TrimSpace(snapshot.Network))
	return snapshot
}

// SetScopeSnapshot records a boundary enforced by the embedding application.
// It deliberately does not apply or validate filesystem scope inside agent.Runtime.
// Unlike model and tool reconfiguration this may happen during an active run;
// already launched work retains the boundary recorded in its own tool details.
func (runtime *Runtime) SetScopeSnapshot(ctx context.Context, scope ScopeSnapshot) error {
	scope = sanitizeScopeSnapshot(scope, runtime.sanitize)
	runtime.configMu.Lock()
	defer runtime.configMu.Unlock()
	if equalScopeSnapshots(scope, runtime.scope) {
		return nil
	}
	records, err := runtime.journal.Records(ctx)
	if err != nil {
		return fmt.Errorf("read journal before scope snapshot: %w", err)
	}
	live, err := reduceRecords(records)
	if err != nil {
		return err
	}
	snapshot := runtime.effectiveConfigSnapshotLocked(runtime.modelInfo, runtime.tools, runtime.toolSet, scope)
	if err := runtime.recordEffectiveConfigurationAndApply(ctx, live, snapshot); err != nil {
		return err
	}
	runtime.scope = scope
	return nil
}

func equalScopeSnapshots(left, right ScopeSnapshot) bool {
	return left.Scope == right.Scope &&
		slices.Equal(left.AddedPaths, right.AddedPaths) &&
		slices.Equal(left.ProtectedPaths, right.ProtectedPaths) &&
		left.Backend == right.Backend &&
		left.Network == right.Network
}
