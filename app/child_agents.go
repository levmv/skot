package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/privatefs"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/internal/toolpolicy"
)

const (
	childMetadataVersion = 1
	childDetailKind      = "agent"

	maxActiveChildren    = 4
	maxChildrenPerParent = 16
	maxRunsPerChild      = 16

	// maxChildResultBytes bounds replies copied into the parent tool result. The
	// complete child trace remains available in its own journal.
	maxChildResultBytes = 32 << 10
)

var childAgentSchema = jsontext.Value(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["start", "send", "check", "stop"]},
    "id": {"type": "string", "description": "Child agent ID for send or stop."},
    "ids": {"type": "array", "items": {"type": "string"}, "description": "Optional child IDs to check; omitted means all."},
    "prompt": {"type": "string", "description": "Self-contained task or follow-up."},
    "model": {"type": "string", "description": "Allowed provider/model override; omitted inherits the parent model."},
    "wait": {"type": "string", "enum": ["none", "any", "all"], "default": "none"}
  },
  "required": ["action"],
  "additionalProperties": false
}`)

type childToolArgs struct {
	Action string   `json:"action"`
	ID     string   `json:"id,omitempty"`
	IDs    []string `json:"ids,omitempty"`
	Prompt string   `json:"prompt,omitempty"`
	Model  string   `json:"model,omitempty"`
	Wait   string   `json:"wait,omitempty"`
}

type childMetadata struct {
	Version         int       `json:"version"`
	AgentID         string    `json:"agent_id"`
	ParentSessionID string    `json:"parent_session_id"`
	SessionID       string    `json:"session_id"`
	Model           string    `json:"model"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	Stopped         bool      `json:"stopped,omitzero"`
}

type childRun struct {
	RunID            string
	Status           agent.RunStatus
	Answer           string
	Error            string
	ToolLimitReached bool
	Usage            agent.ModelUsage
	StartedAt        time.Time
	FinishedAt       time.Time
	Delivered        bool
}

type childAgent struct {
	mu       sync.Mutex
	metadata childMetadata
	dir      string
	journal  *session.Store
	runtime  *agent.Runtime
	runs     []*childRun
	running  bool
	cancel   context.CancelFunc
	done     chan struct{}
}

type childSnapshot struct {
	AgentID         string           `json:"agent_id"`
	ParentSessionID string           `json:"parent_session_id"`
	SessionID       string           `json:"session_id"`
	State           string           `json:"state"`
	Model           string           `json:"model"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	RunID           string           `json:"run_id,omitempty"`
	RunStatus       agent.RunStatus  `json:"run_status,omitempty"`
	Answer          string           `json:"-"`
	Error           string           `json:"-"`
	ToolLimit       bool             `json:"tool_limit_reached,omitzero"`
	Usage           agent.ModelUsage `json:"usage"`
	StartedAt       time.Time        `json:"started_at,omitzero"`
	FinishedAt      time.Time        `json:"finished_at,omitzero"`
	ResultDelivered bool             `json:"result_delivered,omitzero"`
}

// childSupervisor owns nested read-only sessions. It deliberately shares the
// trusted model and workspace adapters with its Application while giving each
// child an independent runtime and journal.
type childSupervisor struct {
	mu sync.Mutex

	home                string
	builder             runtimeBuilder
	instructions        string
	defaultModel        string
	defaultEffort       string
	defaultSelectionAPI string
	defaultContext      int
	allowedModels       map[string]struct{}
	configured          bool
	closed              bool
	children            map[string]map[string]*childAgent
	loaded              map[string]bool
	change              chan struct{}
	lifetime            context.Context
	cancel              context.CancelFunc
}

func newChildSupervisor(home string, configuredModels ...[]string) (*childSupervisor, error) {
	allowed := make(map[string]struct{})
	for _, models := range configuredModels {
		for _, value := range models {
			model, err := normalizeChildModel(value)
			if err != nil {
				return nil, fmt.Errorf("agent model: %w", err)
			}
			if model == "" {
				return nil, errors.New("agent model cannot be empty")
			}
			allowed[model] = struct{}{}
		}
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &childSupervisor{
		home: home, allowedModels: allowed,
		children: make(map[string]map[string]*childAgent), loaded: make(map[string]bool),
		change: make(chan struct{}), lifetime: lifetime, cancel: cancel,
	}, nil
}

func normalizeChildModel(value string) (string, error) {
	provider, model, err := parseModelURI(value)
	if err != nil {
		return "", err
	}
	if _, err := modelProviderSpec(provider); err != nil {
		return "", err
	}
	return provider + "/" + model, nil
}

func (supervisor *childSupervisor) configure(builder runtimeBuilder, instructions, model, effort, selectionAPI string, contextWindow int) error {
	canonicalModel, err := normalizeChildModel(model)
	if err != nil {
		return fmt.Errorf("configure child default model: %w", err)
	}
	readOnly := childReadOnlyToolNames(builder.tools)
	toolSets, err := toolpolicy.NewToolSets(builder.tools, map[string][]string{
		toolpolicy.ToolSetReadOnly: readOnly,
		toolpolicy.ToolSetEdit:     readOnly,
		toolpolicy.ToolSetDefault:  readOnly,
	})
	if err != nil {
		return fmt.Errorf("configure child tool sets: %w", err)
	}
	builder.toolSets = toolSets
	builder.toolSet = toolpolicy.ToolSetReadOnly
	builder.awaitRequiredJobs = false
	builder.externalWork = nil

	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed {
		return errors.New("child supervisor is closed")
	}
	supervisor.builder = builder
	supervisor.instructions = instructions
	supervisor.defaultModel = canonicalModel
	supervisor.defaultEffort = effort
	supervisor.defaultSelectionAPI = selectionAPI
	supervisor.defaultContext = contextWindow
	supervisor.configured = true
	return nil
}

func childReadOnlyToolNames(catalog []agent.Tool) []string {
	allowed := map[string]struct{}{
		"read": {}, "ls": {}, "grep": {}, "glob": {}, "web_fetch": {}, "web_search": {},
	}
	var names []string
	for _, tool := range catalog {
		if _, ok := allowed[tool.Spec.Name]; ok {
			names = append(names, tool.Spec.Name)
		}
	}
	return names
}

func (supervisor *childSupervisor) setModelSelection(model, effort, selectionAPI string, contextWindow int) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.closed {
		supervisor.defaultModel = model
		supervisor.defaultEffort = effort
		supervisor.defaultSelectionAPI = selectionAPI
		supervisor.defaultContext = contextWindow
	}
}

// selectionAPILocked applies the parent's chosen protocol only to the parent's
// own route. A child which selects a different model from the allowlist stands
// on that route's own declaration.
func (supervisor *childSupervisor) selectionAPILocked(model string) string {
	if !strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(supervisor.defaultModel)) {
		return ""
	}
	return supervisor.defaultSelectionAPI
}

func (supervisor *childSupervisor) selectionContextWindowLocked(model string) int {
	if !strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(supervisor.defaultModel)) {
		return 0
	}
	return supervisor.defaultContext
}

func (supervisor *childSupervisor) setSessionDefaults(model, effort, selectionAPI string, contextWindow int, instructions string, scope agent.ScopeSnapshot) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.closed {
		supervisor.defaultModel = model
		supervisor.defaultEffort = effort
		supervisor.defaultSelectionAPI = selectionAPI
		supervisor.defaultContext = contextWindow
		supervisor.instructions = instructions
		supervisor.builder.scope = cloneAgentScopeSnapshot(scope)
	}
}

func (supervisor *childSupervisor) setScopeSnapshot(ctx context.Context, scope agent.ScopeSnapshot) error {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed {
		return errors.New("child supervisor is closed")
	}
	previous := supervisor.builder.scope
	var updated []*agent.Runtime
	for _, group := range supervisor.children {
		for _, child := range group {
			if err := child.runtime.SetScopeSnapshot(ctx, scope); err != nil {
				rollbackCtx := context.WithoutCancel(ctx)
				var rollbackErr error
				for _, runtime := range updated {
					rollbackErr = errors.Join(rollbackErr, runtime.SetScopeSnapshot(rollbackCtx, previous))
				}
				return errors.Join(err, rollbackErr)
			}
			updated = append(updated, child.runtime)
		}
	}
	supervisor.builder.scope = cloneAgentScopeSnapshot(scope)
	return nil
}

func cloneAgentScopeSnapshot(snapshot agent.ScopeSnapshot) agent.ScopeSnapshot {
	snapshot.ProtectedPaths = append([]string(nil), snapshot.ProtectedPaths...)
	snapshot.AddedPaths = append([]string(nil), snapshot.AddedPaths...)
	return snapshot
}

func (supervisor *childSupervisor) tool() agent.Tool {
	return agent.Tool{
		Spec: agent.ToolSpec{
			Name: "agent", Description: "Manage independent read-only child agents: start or continue them asynchronously, check or wait for results, or stop them.",
			InputSchema: childAgentSchema,
		},
		Run: supervisor.runTool,
	}
}

func (supervisor *childSupervisor) runTool(ctx context.Context, raw string) (agent.ToolOutput, error) {
	var args childToolArgs
	if err := decodeChildToolArgs(raw, &args); err != nil {
		return agent.ToolOutput{}, err
	}
	parentID := strings.TrimSpace(agent.ToolSessionID(ctx))
	if !validChildSessionID(parentID) {
		return agent.ToolOutput{}, errors.New("agent tool requires a persisted parent session")
	}

	switch args.Action {
	case "start":
		if err := args.validateStart(); err != nil {
			return agent.ToolOutput{}, err
		}
		child, err := supervisor.start(ctx, parentID, args.Prompt, args.Model)
		if err != nil {
			return agent.ToolOutput{}, err
		}
		snapshot := child.snapshot()
		return childToolOutput("started "+snapshot.AgentID, snapshot)
	case "send":
		if err := args.validateSend(); err != nil {
			return agent.ToolOutput{}, err
		}
		child, err := supervisor.send(ctx, parentID, args.ID, args.Prompt)
		if err != nil {
			return agent.ToolOutput{}, err
		}
		snapshot := child.snapshot()
		return childToolOutput("continued "+snapshot.AgentID, snapshot)
	case "check":
		if err := args.validateCheck(); err != nil {
			return agent.ToolOutput{}, err
		}
		snapshots, err := supervisor.check(ctx, parentID, args.IDs, args.Wait)
		if err != nil {
			return agent.ToolOutput{}, err
		}
		return snapshotsToolOutput(snapshots)
	case "stop":
		if err := args.validateStop(); err != nil {
			return agent.ToolOutput{}, err
		}
		child, err := supervisor.stop(ctx, parentID, args.ID)
		if err != nil {
			return agent.ToolOutput{}, err
		}
		snapshot := child.snapshot()
		return childToolOutput("stopped "+snapshot.AgentID, snapshot)
	default:
		return agent.ToolOutput{}, fmt.Errorf("unknown agent action %q", args.Action)
	}
}

func decodeChildToolArgs(raw string, target *childToolArgs) error {
	if err := agent.DecodeToolArguments(raw, target); err != nil {
		return err
	}
	target.Action = strings.ToLower(strings.TrimSpace(target.Action))
	target.ID = strings.TrimSpace(target.ID)
	target.Model = strings.TrimSpace(target.Model)
	target.Wait = strings.ToLower(strings.TrimSpace(target.Wait))
	for index := range target.IDs {
		target.IDs[index] = strings.TrimSpace(target.IDs[index])
	}
	return nil
}

func (args childToolArgs) validateStart() error {
	if args.ID != "" || len(args.IDs) != 0 || args.Wait != "" {
		return errors.New("agent start accepts only action, prompt, and optional model")
	}
	return validateChildPrompt(args.Prompt)
}

func (args childToolArgs) validateSend() error {
	if !validChildAgentID(args.ID) {
		return errors.New("agent send requires a valid id")
	}
	if len(args.IDs) != 0 || args.Model != "" || args.Wait != "" {
		return errors.New("agent send accepts only action, id, and prompt")
	}
	return validateChildPrompt(args.Prompt)
}

func (args childToolArgs) validateCheck() error {
	if args.ID != "" || args.Prompt != "" || args.Model != "" {
		return errors.New("agent check accepts only action, optional ids, and wait")
	}
	if args.Wait != "" && args.Wait != "none" && args.Wait != "any" && args.Wait != "all" {
		return fmt.Errorf("unknown agent wait mode %q", args.Wait)
	}
	seen := make(map[string]struct{}, len(args.IDs))
	for _, id := range args.IDs {
		if !validChildAgentID(id) {
			return fmt.Errorf("invalid child agent id %q", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("duplicate child agent id %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (args childToolArgs) validateStop() error {
	if !validChildAgentID(args.ID) {
		return errors.New("agent stop requires a valid id")
	}
	if len(args.IDs) != 0 || args.Prompt != "" || args.Model != "" || args.Wait != "" {
		return errors.New("agent stop accepts only action and id")
	}
	return nil
}

func validateChildPrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return errors.New("child agent prompt is required")
	}
	if len(prompt) > productlimits.MaxChildAgentPromptBytes {
		return fmt.Errorf("child agent prompt is %d bytes, limit is %d", len(prompt), productlimits.MaxChildAgentPromptBytes)
	}
	return nil
}

func (supervisor *childSupervisor) start(ctx context.Context, parentID, prompt, requestedModel string) (*childAgent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if err := supervisor.requireReadyLocked(); err != nil {
		return nil, err
	}
	if err := supervisor.loadParentLocked(ctx, parentID); err != nil {
		return nil, err
	}
	group := supervisor.children[parentID]
	if len(group) >= maxChildrenPerParent {
		return nil, fmt.Errorf("child agent limit reached (%d per session)", maxChildrenPerParent)
	}
	if activeChildren(group) >= maxActiveChildren {
		return nil, fmt.Errorf("active child agent limit reached (%d)", maxActiveChildren)
	}
	model, effort, err := supervisor.childModelLocked(requestedModel)
	if err != nil {
		return nil, err
	}
	child, err := supervisor.createChildLocked(ctx, parentID, model, effort)
	if err != nil {
		return nil, err
	}
	if err := supervisor.launchLocked(child, prompt); err != nil {
		return nil, errors.Join(err, cancelAndDiscardNewChild(child))
	}
	if err := publishNewChild(child); err != nil {
		return nil, errors.Join(err, cancelAndDiscardNewChild(child))
	}
	group[child.metadata.AgentID] = child
	supervisor.signalLocked()
	return child, nil
}

func (supervisor *childSupervisor) send(ctx context.Context, parentID, childID, prompt string) (*childAgent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if err := supervisor.requireReadyLocked(); err != nil {
		return nil, err
	}
	if err := supervisor.loadParentLocked(ctx, parentID); err != nil {
		return nil, err
	}
	child, exists := supervisor.children[parentID][childID]
	if !exists {
		return nil, fmt.Errorf("unknown child agent %q", childID)
	}
	if activeChildren(supervisor.children[parentID]) >= maxActiveChildren {
		child.mu.Lock()
		alreadyRunning := child.running
		child.mu.Unlock()
		if !alreadyRunning {
			return nil, fmt.Errorf("active child agent limit reached (%d)", maxActiveChildren)
		}
	}
	if err := supervisor.launchLocked(child, prompt); err != nil {
		return nil, err
	}
	supervisor.signalLocked()
	return child, nil
}

func (supervisor *childSupervisor) childModelLocked(requested string) (string, string, error) {
	if requested == "" {
		return supervisor.defaultModel, supervisor.defaultEffort, nil
	}
	model, err := normalizeChildModel(requested)
	if err != nil {
		return "", "", err
	}
	if model == supervisor.defaultModel {
		return model, supervisor.defaultEffort, nil
	}
	if _, allowed := supervisor.allowedModels[model]; !allowed {
		return "", "", fmt.Errorf("model %q is not allowed for child agents; add it to agent_models", model)
	}
	effort, err := normalizeReasoningEffort(model, "")
	return model, effort, err
}

func (supervisor *childSupervisor) createChildLocked(ctx context.Context, parentID, model, effort string) (*childAgent, error) {
	agentID, err := newChildID("agent", 8)
	if err != nil {
		return nil, err
	}
	sessionID, err := newChildID("session", 16)
	if err != nil {
		return nil, err
	}
	parentDir := filepath.Join(supervisor.home, "agents", parentID)
	if err := privatefs.EnsureDirectory(parentDir, "child agent parent directory"); err != nil {
		return nil, err
	}
	privatefs.TryRestrictPermissions(parentDir)
	stagingDir, err := os.MkdirTemp(parentDir, "."+agentID+"-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temporary child agent directory: %w", err)
	}
	metadata := childMetadata{
		Version: childMetadataVersion, AgentID: agentID, ParentSessionID: parentID,
		SessionID: sessionID, Model: model, ReasoningEffort: effort, CreatedAt: time.Now().UTC(),
	}
	if err := writeChildMetadata(stagingDir, metadata); err != nil {
		_ = os.Remove(stagingDir)
		return nil, err
	}
	journal, err := session.Open(filepath.Join(stagingDir, "events.jsonl"))
	if err != nil {
		_ = os.Remove(filepath.Join(stagingDir, "events.jsonl"))
		_ = os.Remove(filepath.Join(stagingDir, "metadata.json"))
		_ = os.Remove(stagingDir)
		return nil, fmt.Errorf("open child agent journal: %w", err)
	}
	runtime, err := supervisor.builder.build(ctx, runtimeBuildParams{
		journal: journal, sessionID: sessionID, modelURI: model, reasoningEffort: effort,
		modelSelectionAPI: supervisor.selectionAPILocked(model),
		selectionContext:  supervisor.selectionContextWindowLocked(model),
		instructions:      supervisor.instructions, modelOptions: modelBackendOptions{requireCredential: true},
	})
	if err != nil {
		child := &childAgent{metadata: metadata, dir: stagingDir, journal: journal}
		return nil, errors.Join(err, discardNewChild(child))
	}
	return &childAgent{metadata: metadata, dir: stagingDir, journal: journal, runtime: runtime}, nil
}

func (supervisor *childSupervisor) launchLocked(child *childAgent, prompt string) error {
	child.mu.Lock()
	if child.metadata.Stopped {
		child.mu.Unlock()
		return fmt.Errorf("child agent %q is stopped", child.metadata.AgentID)
	}
	if child.running {
		child.mu.Unlock()
		return fmt.Errorf("child agent %q is busy", child.metadata.AgentID)
	}
	if len(child.runs) >= maxRunsPerChild {
		child.mu.Unlock()
		return fmt.Errorf("child agent %q reached its run limit (%d)", child.metadata.AgentID, maxRunsPerChild)
	}
	run := &childRun{StartedAt: time.Now().UTC()}
	child.runs = append(child.runs, run)
	child.running = true
	runCtx, cancel := context.WithCancel(supervisor.lifetime)
	child.cancel = cancel
	child.done = make(chan struct{})
	done := child.done
	runtime := child.runtime
	child.mu.Unlock()

	started := make(chan error, 1)
	var startOnce sync.Once
	signalStarted := func(err error) { startOnce.Do(func() { started <- err }) }
	go func() {
		result, runErr := runtime.Run(runCtx, prompt, func(event agent.Event) {
			if event.Kind == agent.EventRunStarted && event.RunID != "" {
				child.mu.Lock()
				run.RunID = event.RunID
				child.mu.Unlock()
				signalStarted(nil)
			}
		})
		child.mu.Lock()
		run.RunID = result.RunID
		run.Status = result.Status
		run.Answer = truncateUTF8(result.Answer, maxChildResultBytes)
		run.ToolLimitReached = result.ToolLimitReached
		if runErr != nil {
			run.Error = truncateUTF8(runErr.Error(), maxChildResultBytes)
		}
		run.FinishedAt = time.Now().UTC()
		if state, err := runtime.State(context.WithoutCancel(runCtx)); err == nil {
			previous := agent.ModelUsage{}
			if len(child.runs) > 1 {
				for _, earlier := range child.runs[:len(child.runs)-1] {
					previous = previous.Add(earlier.Usage)
				}
			}
			run.Usage = subtractModelUsage(state.Usage, previous)
		}
		child.running = false
		child.cancel = nil
		close(done)
		child.mu.Unlock()
		if runErr != nil {
			signalStarted(fmt.Errorf("start child agent run: %w", runErr))
		} else if result.RunID != "" {
			signalStarted(nil)
		} else {
			signalStarted(errors.New("child agent run did not start"))
		}
		supervisor.signal()
	}()
	startErr := <-started
	if startErr != nil {
		child.mu.Lock()
		if len(child.runs) != 0 && child.runs[len(child.runs)-1] == run && run.RunID == "" {
			child.runs = child.runs[:len(child.runs)-1]
		}
		child.mu.Unlock()
	}
	return startErr
}

func publishNewChild(child *childAgent) error {
	child.mu.Lock()
	stagingDir := child.dir
	finalDir := filepath.Join(filepath.Dir(stagingDir), child.metadata.AgentID)
	child.mu.Unlock()
	if err := os.Rename(stagingDir, finalDir); err != nil {
		return fmt.Errorf("publish child agent directory: %w", err)
	}
	child.mu.Lock()
	child.dir = finalDir
	child.mu.Unlock()
	return nil
}

func subtractModelUsage(total, previous agent.ModelUsage) agent.ModelUsage {
	return agent.ModelUsage{
		InputTokens:       max(0, total.InputTokens-previous.InputTokens),
		CachedInputTokens: max(0, total.CachedInputTokens-previous.CachedInputTokens),
		OutputTokens:      max(0, total.OutputTokens-previous.OutputTokens),
		ReasoningTokens:   max(0, total.ReasoningTokens-previous.ReasoningTokens),
		TotalTokens:       max(0, total.TotalTokens-previous.TotalTokens),
	}
}

func (supervisor *childSupervisor) check(ctx context.Context, parentID string, ids []string, waitMode string) ([]childSnapshot, error) {
	if waitMode == "" {
		waitMode = "none"
	}
	for {
		watch := supervisor.watch()
		supervisor.mu.Lock()
		if err := supervisor.requireReadyLocked(); err != nil {
			supervisor.mu.Unlock()
			return nil, err
		}
		if err := supervisor.loadParentLocked(ctx, parentID); err != nil {
			supervisor.mu.Unlock()
			return nil, err
		}
		selected, err := selectChildren(supervisor.children[parentID], ids)
		supervisor.mu.Unlock()
		if err != nil {
			return nil, err
		}
		snapshots := snapshotChildren(selected)
		if waitSatisfied(snapshots, waitMode) {
			return snapshots, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-watch:
		}
	}
}

func waitSatisfied(snapshots []childSnapshot, mode string) bool {
	if mode == "none" || len(snapshots) == 0 {
		return true
	}
	if mode == "any" {
		for _, snapshot := range snapshots {
			if snapshot.State != "running" {
				return true
			}
		}
		return false
	}
	for _, snapshot := range snapshots {
		if snapshot.State == "running" {
			return false
		}
	}
	return true
}

func selectChildren(group map[string]*childAgent, ids []string) ([]*childAgent, error) {
	if len(ids) == 0 {
		children := make([]*childAgent, 0, len(group))
		for _, child := range group {
			children = append(children, child)
		}
		return children, nil
	}
	children := make([]*childAgent, 0, len(ids))
	for _, id := range ids {
		child, exists := group[id]
		if !exists {
			return nil, fmt.Errorf("unknown child agent %q", id)
		}
		children = append(children, child)
	}
	return children, nil
}

func snapshotChildren(children []*childAgent) []childSnapshot {
	snapshots := make([]childSnapshot, 0, len(children))
	for _, child := range children {
		snapshots = append(snapshots, child.snapshot())
	}
	sort.Slice(snapshots, func(left, right int) bool { return snapshots[left].AgentID < snapshots[right].AgentID })
	return snapshots
}

func (child *childAgent) snapshot() childSnapshot {
	child.mu.Lock()
	defer child.mu.Unlock()
	state := "idle"
	if child.metadata.Stopped {
		state = "stopped"
	} else if child.running {
		state = "running"
	}
	snapshot := childSnapshot{
		AgentID: child.metadata.AgentID, ParentSessionID: child.metadata.ParentSessionID,
		SessionID: child.metadata.SessionID, State: state, Model: child.metadata.Model,
		ReasoningEffort: child.metadata.ReasoningEffort,
	}
	if len(child.runs) != 0 {
		run := child.runs[len(child.runs)-1]
		snapshot.RunID = run.RunID
		snapshot.RunStatus = run.Status
		snapshot.Answer = run.Answer
		snapshot.Error = run.Error
		snapshot.ToolLimit = run.ToolLimitReached
		snapshot.Usage = run.Usage
		snapshot.StartedAt = run.StartedAt
		snapshot.FinishedAt = run.FinishedAt
	}
	return snapshot
}

func (supervisor *childSupervisor) stop(ctx context.Context, parentID, childID string) (*childAgent, error) {
	supervisor.mu.Lock()
	if err := supervisor.requireReadyLocked(); err != nil {
		supervisor.mu.Unlock()
		return nil, err
	}
	if err := supervisor.loadParentLocked(ctx, parentID); err != nil {
		supervisor.mu.Unlock()
		return nil, err
	}
	child, exists := supervisor.children[parentID][childID]
	supervisor.mu.Unlock()
	if !exists {
		return nil, fmt.Errorf("unknown child agent %q", childID)
	}

	child.mu.Lock()
	wasStopped := child.metadata.Stopped
	child.metadata.Stopped = true
	cancel, done := child.cancel, child.done
	metadata := child.metadata
	child.mu.Unlock()
	if err := writeChildMetadata(child.dir, metadata); err != nil {
		child.mu.Lock()
		child.metadata.Stopped = wasStopped
		child.mu.Unlock()
		return nil, err
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	supervisor.signal()
	return child, nil
}

func (supervisor *childSupervisor) requireReadyLocked() error {
	if supervisor.closed {
		return errors.New("child supervisor is closed")
	}
	if !supervisor.configured {
		return errors.New("child supervisor is not configured")
	}
	return nil
}

func (supervisor *childSupervisor) loadParentLocked(ctx context.Context, parentID string) error {
	return supervisor.loadParentWithLocked(ctx, parentID, supervisor.builder, supervisor.instructions)
}

func (supervisor *childSupervisor) loadParentWithLocked(ctx context.Context, parentID string, builder runtimeBuilder, instructions string) error {
	if supervisor.loaded[parentID] {
		return nil
	}
	group := make(map[string]*childAgent)
	dir := filepath.Join(supervisor.home, "agents", parentID)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		supervisor.children[parentID] = group
		supervisor.loaded[parentID] = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("list child agents: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validChildAgentID(entry.Name()) {
			continue
		}
		child, err := supervisor.openChildLocked(ctx, filepath.Join(dir, entry.Name()), parentID, entry.Name(), builder, instructions)
		if err != nil {
			for _, opened := range group {
				_ = opened.journal.Close()
			}
			return fmt.Errorf("open child agent %s: %w", entry.Name(), err)
		}
		group[entry.Name()] = child
	}
	supervisor.children[parentID] = group
	supervisor.loaded[parentID] = true
	return nil
}

func (supervisor *childSupervisor) Preload(ctx context.Context, parentID string) error {
	supervisor.mu.Lock()
	builder, instructions := supervisor.builder, supervisor.instructions
	supervisor.mu.Unlock()
	return supervisor.preloadWith(ctx, parentID, builder, instructions)
}

func (supervisor *childSupervisor) PreloadSession(ctx context.Context, parentID, instructions string, scope agent.ScopeSnapshot) error {
	supervisor.mu.Lock()
	builder := supervisor.builder
	supervisor.mu.Unlock()
	builder.scope = cloneAgentScopeSnapshot(scope)
	return supervisor.preloadWith(ctx, parentID, builder, instructions)
}

func (supervisor *childSupervisor) preloadWith(ctx context.Context, parentID string, builder runtimeBuilder, instructions string) error {
	if parentID == "" {
		return nil
	}
	if !validChildSessionID(parentID) {
		return fmt.Errorf("invalid parent session ID %q", parentID)
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if err := supervisor.requireReadyLocked(); err != nil {
		return err
	}
	return supervisor.loadParentWithLocked(ctx, parentID, builder, instructions)
}

func (supervisor *childSupervisor) openChildLocked(ctx context.Context, dir, parentID, agentID string, builder runtimeBuilder, instructions string) (*childAgent, error) {
	metadata, err := readChildMetadata(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return nil, err
	}
	if metadata.Version != childMetadataVersion || metadata.ParentSessionID != parentID || metadata.AgentID != agentID || !validChildSessionID(metadata.SessionID) || metadata.CreatedAt.IsZero() {
		return nil, errors.New("child metadata identity is invalid")
	}
	model, err := normalizeChildModel(metadata.Model)
	if err != nil || model != metadata.Model {
		return nil, errors.New("child metadata model is invalid")
	}
	journalPath := filepath.Join(dir, "events.jsonl")
	if err := inspectChildStateFile(journalPath, "journal"); err != nil {
		return nil, err
	}
	journal, err := session.Open(journalPath)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*childAgent, error) {
		return nil, errors.Join(cause, journal.Close())
	}
	sessionState, records, err := agent.Reconcile(ctx, journal)
	if err != nil {
		return fail(fmt.Errorf("reconcile journal: %w", err))
	}
	runs := restoreChildRuns(records, sessionState.Blocks)
	modelURI := strings.TrimSpace(metadata.Model)
	reasoningEffort := strings.TrimSpace(metadata.ReasoningEffort)
	if sessionState.Selection.Model != "" {
		modelURI = strings.TrimSpace(sessionState.Selection.Provider) + "/" + strings.TrimSpace(sessionState.Selection.Model)
		reasoningEffort = strings.TrimSpace(sessionState.Selection.ReasoningEffort)
	}
	metadata.Model = modelURI
	metadata.ReasoningEffort = reasoningEffort
	params := runtimeBuildParams{
		journal: journal, sessionID: metadata.SessionID, modelURI: modelURI,
		reasoningEffort: reasoningEffort, instructions: instructions, resumedState: &sessionState,
	}
	if modelInfo, ok := restoredModelInfo(sessionState, modelURI); ok {
		params.knownModel = &modelInfo
	}
	runtime, err := builder.buildRestored(ctx, params)
	if err != nil {
		return fail(err)
	}
	return &childAgent{metadata: metadata, dir: dir, journal: journal, runtime: runtime, runs: runs}, nil
}

func restoreChildRuns(records []agent.Record, blocks []agent.ConversationBlock) []*childRun {
	runs := make([]*childRun, 0, len(blocks))
	recordIndex := 0
	for _, block := range blocks {
		run := &childRun{
			RunID: block.RunID, Status: block.Status,
			Error:            truncateUTF8(block.Error, maxChildResultBytes),
			ToolLimitReached: block.ToolLimitReached,
		}
		// Start at the preceding block boundary so compaction performed before
		// RunStarted contributes its usage to the run that follows it.
		for recordIndex < len(records) && records[recordIndex].Sequence <= block.EndSequence {
			record := records[recordIndex]
			switch record.Kind {
			case agent.RecordRunStarted:
				var payload agent.RunStartedRecord
				if json.Unmarshal(record.Data, &payload) == nil && payload.RunID == block.RunID {
					run.StartedAt = record.Time
				}
			case agent.RecordModelResponse:
				var payload agent.ModelResponseRecord
				if json.Unmarshal(record.Data, &payload) == nil && payload.RunID == block.RunID {
					run.Usage = run.Usage.Add(payload.Usage)
					run.Answer = truncateUTF8(assistantReply(payload.Items), maxChildResultBytes)
				}
			case agent.RecordContextCompacted:
				var payload agent.ContextCompactedRecord
				if json.Unmarshal(record.Data, &payload) == nil {
					run.Usage = run.Usage.Add(payload.Usage)
				}
			case agent.RecordRunFinished:
				var payload agent.RunFinishedRecord
				if json.Unmarshal(record.Data, &payload) == nil && payload.RunID == block.RunID {
					run.FinishedAt = record.Time
				}
			}
			recordIndex++
		}
		runs = append(runs, run)
	}
	return runs
}

func assistantReply(items []agent.Item) string {
	var parts []string
	for _, item := range items {
		if item.Kind == agent.ItemAssistantText && strings.TrimSpace(item.Text) != "" {
			parts = append(parts, strings.TrimSpace(item.Text))
		}
	}
	return strings.Join(parts, "\n")
}

func activeChildren(group map[string]*childAgent) int {
	active := 0
	for _, child := range group {
		child.mu.Lock()
		if child.running {
			active++
		}
		child.mu.Unlock()
	}
	return active
}

func (supervisor *childSupervisor) HasChildren(parentID string) bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return len(supervisor.children[parentID]) != 0
}

func (supervisor *childSupervisor) Status(id string) ([]agent.Detail, bool) {
	if !validChildAgentID(id) {
		return nil, false
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	for _, group := range supervisor.children {
		if child, exists := group[id]; exists {
			detail, err := snapshotDetail(child.snapshot())
			if err != nil {
				return nil, false
			}
			return []agent.Detail{detail}, true
		}
	}
	return nil, false
}

func (supervisor *childSupervisor) PendingEvents(parentID string) []agent.BoundaryEvent {
	supervisor.mu.Lock()
	group := supervisor.children[parentID]
	children := make([]*childAgent, 0, len(group))
	for _, child := range group {
		children = append(children, child)
	}
	supervisor.mu.Unlock()

	var events []agent.BoundaryEvent
	for _, child := range children {
		child.mu.Lock()
		for _, run := range child.runs {
			if run.RunID == "" || run.FinishedAt.IsZero() || run.Delivered {
				continue
			}
			events = append(events, agent.BoundaryEvent{
				JobID: childEventID(child.metadata.AgentID, run.RunID), FinishedAt: run.FinishedAt,
				Content: childCompletionContent(child.metadata.AgentID, run),
			})
		}
		child.mu.Unlock()
	}
	sort.Slice(events, func(left, right int) bool {
		if events[left].FinishedAt.Equal(events[right].FinishedAt) {
			return events[left].JobID < events[right].JobID
		}
		return events[left].FinishedAt.Before(events[right].FinishedAt)
	})
	return events
}

func (supervisor *childSupervisor) EventCommitted(eventID string) {
	agentID, runID, ok := parseChildEventID(eventID)
	if !ok {
		return
	}
	supervisor.markDelivered(agentID, runID)
}

func (supervisor *childSupervisor) ToolResultCommitted(result agent.ToolResult) {
	for _, detail := range result.Details {
		if detail.Kind != childDetailKind {
			continue
		}
		var snapshot childSnapshot
		if json.Unmarshal(detail.Data, &snapshot) == nil && snapshot.ResultDelivered && snapshot.AgentID != "" && snapshot.RunID != "" && snapshot.State != "running" {
			supervisor.markDelivered(snapshot.AgentID, snapshot.RunID)
		}
	}
}

func (supervisor *childSupervisor) markDelivered(agentID, runID string) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	for _, group := range supervisor.children {
		child, exists := group[agentID]
		if !exists {
			continue
		}
		child.mu.Lock()
		for _, run := range child.runs {
			if run.RunID == runID {
				run.Delivered = true
				break
			}
		}
		child.mu.Unlock()
		return
	}
}

func childEventID(agentID, runID string) string { return "agent:" + agentID + ":" + runID }

func parseChildEventID(value string) (string, string, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] != "agent" || !validChildAgentID(parts[1]) || !strings.HasPrefix(parts[2], "run_") {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func childCompletionContent(agentID string, run *childRun) string {
	status := string(run.Status)
	if status == "" {
		status = "finished"
	}
	content := fmt.Sprintf("Child agent %s %s", agentID, status)
	if run.Usage.TotalTokens > 0 {
		content += fmt.Sprintf(" (%d tokens)", run.Usage.TotalTokens)
	}
	preview := childResultText(run.Status, run.Answer, run.Error)
	preview = truncateUTF8(strings.TrimSpace(preview), 600)
	if preview != "" {
		content += ": " + preview
	}
	return content
}

func childToolOutput(prefix string, snapshot childSnapshot) (agent.ToolOutput, error) {
	content := prefix
	if snapshot.RunID != "" {
		content += " (" + snapshot.RunID + ")"
	}
	detail, err := snapshotDetail(snapshot)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	return agent.ToolOutput{Content: agent.TextContent(content), Details: []agent.Detail{detail}}, nil
}

func snapshotsToolOutput(snapshots []childSnapshot) (agent.ToolOutput, error) {
	if len(snapshots) == 0 {
		return agent.ToolOutput{Content: agent.TextContent("no child agents")}, nil
	}
	var output strings.Builder
	var details []agent.Detail
	remaining := maxChildResultBytes
	for index, snapshot := range snapshots {
		if index != 0 {
			output.WriteString("\n\n")
		}
		line := snapshot.AgentID + " " + snapshot.State
		if snapshot.RunID != "" {
			line += " " + snapshot.RunID
		}
		if snapshot.RunStatus != "" {
			line += " " + string(snapshot.RunStatus)
		}
		if snapshot.ToolLimit {
			line += " · tool limit"
		}
		if snapshot.Usage.TotalTokens > 0 {
			line += fmt.Sprintf(" · %d tokens", snapshot.Usage.TotalTokens)
		}
		output.WriteString(line)
		remaining -= len(line) + 2
		result := childResultText(snapshot.RunStatus, snapshot.Answer, snapshot.Error)
		if result != "" && remaining > 0 {
			result = truncateUTF8(strings.TrimSpace(result), remaining)
			output.WriteString("\n" + result)
			remaining -= len(result) + 1
		}
		snapshot.ResultDelivered = true
		detail, err := snapshotDetail(snapshot)
		if err != nil {
			return agent.ToolOutput{}, err
		}
		details = append(details, detail)
	}
	return agent.ToolOutput{Content: agent.TextContent(output.String()), Details: details}, nil
}

func childResultText(status agent.RunStatus, answer, runError string) string {
	if status == agent.RunCompleted || status == agent.RunIncomplete {
		if answer != "" {
			return answer
		}
		return runError
	}
	if runError != "" {
		return runError
	}
	return answer
}

func snapshotDetail(snapshot childSnapshot) (agent.Detail, error) {
	return agent.NewDetail(childDetailKind, snapshot)
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	if maxBytes <= len("…") {
		return ""
	}
	value = value[:maxBytes-len("…")]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "…"
}

func (supervisor *childSupervisor) signal() {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	supervisor.signalLocked()
}

func (supervisor *childSupervisor) signalLocked() {
	close(supervisor.change)
	supervisor.change = make(chan struct{})
}

func (supervisor *childSupervisor) watch() <-chan struct{} {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return supervisor.change
}

func (supervisor *childSupervisor) ReleaseParent(parentID string) error {
	if parentID == "" {
		return nil
	}
	supervisor.mu.Lock()
	group := supervisor.children[parentID]
	delete(supervisor.children, parentID)
	delete(supervisor.loaded, parentID)
	supervisor.signalLocked()
	supervisor.mu.Unlock()
	return closeChildren(group)
}

func (supervisor *childSupervisor) Close() error {
	if supervisor == nil {
		return nil
	}
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return nil
	}
	supervisor.closed = true
	supervisor.cancel()
	groups := supervisor.children
	supervisor.children = make(map[string]map[string]*childAgent)
	supervisor.loaded = make(map[string]bool)
	supervisor.signalLocked()
	supervisor.mu.Unlock()
	var closeErr error
	for _, group := range groups {
		closeErr = errors.Join(closeErr, closeChildren(group))
	}
	return closeErr
}

func closeChildren(group map[string]*childAgent) error {
	children := make([]*childAgent, 0, len(group))
	for _, child := range group {
		children = append(children, child)
		child.mu.Lock()
		cancel, done := child.cancel, child.done
		child.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
	}
	var closeErr error
	for _, child := range children {
		closeErr = errors.Join(closeErr, child.journal.Close())
	}
	return closeErr
}

func discardNewChild(child *childAgent) error {
	if child == nil {
		return nil
	}
	var cleanupErr error
	if child.journal != nil {
		cleanupErr = errors.Join(cleanupErr, child.journal.Close())
	}
	for _, name := range []string{"events.jsonl", "metadata.json"} {
		if err := os.Remove(filepath.Join(child.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if err := os.Remove(child.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	return cleanupErr
}

func cancelAndDiscardNewChild(child *childAgent) error {
	if child == nil {
		return nil
	}
	child.mu.Lock()
	cancel, done := child.cancel, child.done
	child.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return discardNewChild(child)
}

func writeChildMetadata(dir string, metadata childMetadata) error {
	raw, err := json.Marshal(metadata, json.Deterministic(true), jsontext.WithIndentPrefix(""), jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("encode child agent metadata: %w", err)
	}
	file, err := os.CreateTemp(dir, ".metadata-*.tmp")
	if err != nil {
		return fmt.Errorf("create child agent metadata: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	fail := func(operation string, cause error) error {
		_ = file.Close()
		return fmt.Errorf("%s child agent metadata: %w", operation, cause)
	}
	if err := file.Chmod(0o600); err != nil {
		return fail("restrict", err)
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return fail("write", err)
	}
	if err := file.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close child agent metadata: %w", err)
	}
	if err := os.Rename(temporary, filepath.Join(dir, "metadata.json")); err != nil {
		return fmt.Errorf("replace child agent metadata: %w", err)
	}
	return nil
}

func readChildMetadata(path string) (childMetadata, error) {
	if err := inspectChildStateFile(path, "metadata"); err != nil {
		return childMetadata{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return childMetadata{}, fmt.Errorf("read metadata: %w", err)
	}
	var metadata childMetadata
	if err := json.Unmarshal(raw, &metadata, json.RejectUnknownMembers(true)); err != nil {
		return childMetadata{}, fmt.Errorf("decode metadata: %w", err)
	}
	return metadata, nil
}

func inspectChildStateFile(path, label string) error {
	if err := privatefs.RequireRegularFile(path, "child agent "+label); err != nil {
		return err
	}
	privatefs.TryRestrictPermissions(path)
	return nil
}

func newChildID(prefix string, bytesCount int) (string, error) {
	data := make([]byte, bytesCount)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate child agent ID: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(data), nil
}

func validChildSessionID(id string) bool {
	return validHexID(id, "session", 16)
}

func validChildAgentID(id string) bool {
	return validHexID(id, "agent", 8)
}

func validHexID(id, prefix string, bytesCount int) bool {
	wantPrefix := prefix + "_"
	if len(id) != len(wantPrefix)+bytesCount*2 || !strings.HasPrefix(id, wantPrefix) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, wantPrefix))
	return err == nil
}
