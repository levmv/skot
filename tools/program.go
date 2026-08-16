package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
)

const (
	defaultProgramTimeout = 10 * time.Minute
	maxProgramTimeout     = time.Hour
	programBackgroundArg  = "background"
)

var programToolNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,63}$`)

// BackgroundMode decides whether a configured program tool may leave work
// running after its call returns. Never is the zero-value default. Auto adds a
// boolean background argument to the model-visible schema; always starts every
// call as a managed job.
type BackgroundMode string

const (
	BackgroundNever  BackgroundMode = "never"
	BackgroundAuto   BackgroundMode = "auto"
	BackgroundAlways BackgroundMode = "always"
)

// ProgramTool declares one executable-backed model tool. The JSON tags are a
// user-facing file format; arguments are passed to Command on stdin as one JSON
// object, stdout is semantic output, and stderr remains diagnostic output.
type ProgramTool struct {
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Command      []string          `json:"command"`
	Parameters   json.RawMessage   `json:"parameters"`
	Timeout      int               `json:"timeout,omitempty"`
	Workdir      string            `json:"workdir,omitempty"`
	ParallelSafe bool              `json:"parallel_safe,omitempty"`
	Background   BackgroundMode    `json:"background,omitempty"`
	Yield        int               `json:"yield,omitempty"`
	Detach       bool              `json:"detach,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
}

// ProgramToolConfig is the top-level tools.json document.
type ProgramToolConfig struct {
	Tools []ProgramTool `json:"tools"`
}

// ResolvedProgramTool keeps a runnable tool and the secret-free description of
// how it will execute together, so callers cannot accidentally let the model
// catalog and the journal snapshot drift apart.
type ResolvedProgramTool struct {
	Tool     agent.Tool
	Snapshot agent.ProgramToolSnapshot
	// CanBackground means any tool set containing Tool must also contain job.
	CanBackground bool
}

// LoadProgramTools reads and validates a tools.json file. A missing file is an
// empty catalog; an existing malformed file is a configuration error.
func LoadProgramTools(path string) (ProgramToolConfig, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ProgramToolConfig{}, nil
	}
	if err != nil {
		return ProgramToolConfig{}, fmt.Errorf("read tool config %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config ProgramToolConfig
	if err := decoder.Decode(&config); err != nil {
		return ProgramToolConfig{}, fmt.Errorf("parse tool config %s: %w", path, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ProgramToolConfig{}, fmt.Errorf("parse tool config %s: multiple JSON values", path)
	}
	seen := make(map[string]struct{}, len(config.Tools))
	for index := range config.Tools {
		tool := &config.Tools[index]
		label := strings.TrimSpace(tool.Name)
		if label == "" {
			label = fmt.Sprintf("entry %d", index+1)
		}
		if err := tool.normalize(); err != nil {
			return ProgramToolConfig{}, fmt.Errorf("tool %s in %s: %w", label, path, err)
		}
		if _, exists := seen[tool.Name]; exists {
			return ProgramToolConfig{}, fmt.Errorf("tool config %s declares %q twice", path, tool.Name)
		}
		seen[tool.Name] = struct{}{}
	}
	return config, nil
}

func (tool *ProgramTool) normalize() error {
	tool.Name = strings.TrimSpace(tool.Name)
	if !programToolNamePattern.MatchString(tool.Name) {
		return errors.New("name must start with a letter and contain only letters, digits, and underscores")
	}
	tool.Description = strings.TrimSpace(tool.Description)
	if tool.Description == "" {
		return errors.New("description is required")
	}
	if len(tool.Command) == 0 || strings.TrimSpace(tool.Command[0]) == "" {
		return errors.New("command must name a program to run")
	}
	tool.Command = slices.Clone(tool.Command)
	tool.Command[0] = strings.TrimSpace(tool.Command[0])
	if strings.IndexByte(tool.Command[0], 0) >= 0 {
		return errors.New("command program contains a NUL byte")
	}
	for index, argument := range tool.Command[1:] {
		if strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("command argument %d contains a NUL byte", index+2)
		}
	}
	parameters, err := normalizeProgramSchema(tool.Parameters)
	if err != nil {
		return err
	}
	tool.Parameters = parameters
	if tool.Timeout < 0 {
		return errors.New("timeout cannot be negative")
	}
	if tool.Timeout > int(maxProgramTimeout/time.Second) {
		return fmt.Errorf("timeout cannot exceed %d seconds", int(maxProgramTimeout/time.Second))
	}
	tool.Workdir = strings.TrimSpace(tool.Workdir)
	if filepath.IsAbs(tool.Workdir) {
		return fmt.Errorf("workdir %q must be relative to the workspace root", tool.Workdir)
	}
	if tool.Background == "" {
		tool.Background = BackgroundNever
	}
	switch tool.Background {
	case BackgroundNever, BackgroundAlways:
	case BackgroundAuto:
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
			return fmt.Errorf("decode parameters: %w", err)
		}
		if _, exists := schema.Properties[programBackgroundArg]; exists {
			return errors.New("background auto adds a background parameter and parameters already declares one")
		}
	default:
		return fmt.Errorf("background %q must be one of never, auto, always", tool.Background)
	}
	if tool.Yield < 0 {
		return errors.New("yield cannot be negative")
	}
	if tool.Yield > int(maxProgramTimeout/time.Second) {
		return fmt.Errorf("yield cannot exceed %d seconds", int(maxProgramTimeout/time.Second))
	}
	if tool.Yield > 0 && tool.Background == BackgroundAlways {
		return errors.New("yield applies to foreground calls and cannot be combined with background always")
	}
	if tool.Yield > 0 && time.Duration(tool.Yield)*time.Second >= tool.timeout() {
		return errors.New("yield must be shorter than the effective timeout")
	}
	if tool.Detach && tool.Background == BackgroundNever && tool.Yield == 0 {
		return errors.New("detach requires background auto/always or a positive yield")
	}
	if err := validateProgramEnv(tool.Env); err != nil {
		return err
	}
	tool.Env = maps.Clone(tool.Env)
	return nil
}

func normalizeProgramSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{"type":"object"}`)
	}
	var schema map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&schema); err != nil {
		return nil, fmt.Errorf("parameters must be a JSON Schema object: %w", err)
	}
	if schema == nil {
		return nil, errors.New("parameters must be a JSON Schema object")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("parameters contains multiple JSON values")
	}
	if kind, exists := schema["type"]; exists {
		var name string
		if err := json.Unmarshal(kind, &name); err != nil || name != "object" {
			return nil, errors.New("parameters must have type object")
		}
	} else {
		schema["type"] = json.RawMessage(`"object"`)
	}
	normalized, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("normalize parameters: %w", err)
	}
	return normalized, nil
}

func validateProgramEnv(environment map[string]string) error {
	for name := range environment {
		switch {
		case strings.TrimSpace(name) == "" || strings.ContainsRune(name, '='):
			return fmt.Errorf("env name %q is not a variable name", name)
		case strings.IndexByte(name, 0) >= 0:
			return fmt.Errorf("env name %q contains a NUL byte", name)
		case name == "HOME" || name == "TMPDIR":
			return fmt.Errorf("env %s is managed by the execution environment", name)
		case strings.HasPrefix(name, "SK_INTERNAL_"):
			return fmt.Errorf("env %s is reserved", name)
		}
		if strings.IndexByte(environment[name], 0) >= 0 {
			return fmt.Errorf("env %s contains a NUL byte", name)
		}
	}
	return nil
}

func (tool ProgramTool) timeout() time.Duration {
	if tool.Timeout <= 0 {
		return defaultProgramTimeout
	}
	return time.Duration(tool.Timeout) * time.Second
}

func (tool ProgramTool) schema() json.RawMessage {
	if tool.Background != BackgroundAuto {
		return slices.Clone(tool.Parameters)
	}
	var schema map[string]json.RawMessage
	_ = json.Unmarshal(tool.Parameters, &schema)
	var properties map[string]json.RawMessage
	if raw := schema["properties"]; len(raw) != 0 {
		_ = json.Unmarshal(raw, &properties)
	}
	if properties == nil {
		properties = make(map[string]json.RawMessage)
	}
	properties[programBackgroundArg] = json.RawMessage(`{"type":"boolean","description":"Return a job id immediately and keep the tool running through a durable worker. Use it only when this reply does not need the result."}`)
	schema["properties"], _ = json.Marshal(properties)
	result, _ := json.Marshal(schema)
	return result
}

// ResolveProgramTools resolves every executable now and produces ordinary
// agent tools. Name collisions are deliberately left to the complete catalog
// validation performed by app.Open.
func (manager *ProcessManager) ResolveProgramTools(declarations []ProgramTool) ([]ResolvedProgramTool, error) {
	resolved := make([]ResolvedProgramTool, 0, len(declarations))
	for _, declaration := range declarations {
		if err := declaration.normalize(); err != nil {
			return nil, fmt.Errorf("tool %s: %w", strings.TrimSpace(declaration.Name), err)
		}
		program, err := manager.resolveProgram(declaration.Command[0])
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", declaration.Name, err)
		}
		envNames := make([]string, 0, len(declaration.Env))
		for name := range declaration.Env {
			envNames = append(envNames, name)
		}
		slices.Sort(envNames)
		snapshot := agent.ProgramToolSnapshot{
			Name:             declaration.Name,
			Program:          program,
			Command:          slices.Clone(declaration.Command),
			Workdir:          declaration.Workdir,
			Timeout:          declaration.timeout().String(),
			ParallelSafe:     declaration.ParallelSafe,
			Background:       string(declaration.Background),
			Detach:           declaration.Detach,
			EnvironmentNames: envNames,
		}
		if declaration.Yield > 0 {
			snapshot.Yield = (time.Duration(declaration.Yield) * time.Second).String()
		}
		declaration := declaration
		resolved = append(resolved, ResolvedProgramTool{
			Tool: agent.Tool{
				Spec: agent.ToolSpec{
					Name:         declaration.Name,
					Description:  declaration.Description,
					InputSchema:  declaration.schema(),
					ParallelSafe: declaration.ParallelSafe,
				},
				Run: manager.programRunner(declaration, program),
			},
			Snapshot:      snapshot,
			CanBackground: declaration.Background != BackgroundNever || declaration.Yield > 0,
		})
	}
	return resolved, nil
}

func (manager *ProcessManager) resolveProgram(name string) (string, error) {
	policy := manager.access.snapshot()
	if !strings.ContainsRune(name, filepath.Separator) && !strings.ContainsRune(name, '/') {
		resolved, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("program %q is not on PATH: %w", name, err)
		}
		resolved = canonicalSandboxPath(resolved)
		if policy.protection.Protects(resolved) {
			return "", fmt.Errorf("program %q is protected", name)
		}
		return resolved, nil
	}
	resolved := name
	if !filepath.IsAbs(resolved) {
		absolute, _, info, err := policy.workspaceOnly().resolveExistingPath(resolved, true)
		if err != nil {
			return "", fmt.Errorf("program %q: %w", name, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("program %q is a directory", name)
		}
		resolved = absolute
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("program %q: %w", name, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("program %q is not executable", name)
	}
	if policy.protection.Protects(resolved) {
		return "", fmt.Errorf("program %q is protected", name)
	}
	return resolved, nil
}

func validateProgramArguments(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "{}"
	}
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("invalid tool arguments: %w", err)
	}
	if object == nil {
		return nil, errors.New("invalid tool arguments: expected one JSON object")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid tool arguments: multiple JSON values")
	}
	return []byte(raw), nil
}

func takeProgramBackground(raw []byte) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false, fmt.Errorf("invalid tool arguments: %w", err)
	}
	value, exists := fields[programBackgroundArg]
	if !exists {
		return raw, false, nil
	}
	var background bool
	if err := json.Unmarshal(value, &background); err != nil {
		return nil, false, errors.New("invalid tool arguments: background must be true or false")
	}
	delete(fields, programBackgroundArg)
	trimmed, err := json.Marshal(fields)
	return trimmed, background, err
}

func (manager *ProcessManager) programRunner(declaration ProgramTool, program string) func(context.Context, string) (agent.ToolOutput, error) {
	return func(ctx context.Context, raw string) (agent.ToolOutput, error) {
		arguments, err := validateProgramArguments(raw)
		if err != nil {
			return agent.ToolOutput{}, err
		}
		background := declaration.Background == BackgroundAlways
		if declaration.Background == BackgroundAuto {
			arguments, background, err = takeProgramBackground(arguments)
			if err != nil {
				return agent.ToolOutput{}, err
			}
		}
		if err := ctx.Err(); err != nil {
			return agent.ToolOutput{}, err
		}
		job, err := manager.start(processSpec{
			command:        declaration.Name,
			timeout:        declaration.timeout(),
			origin:         processOriginModel,
			sessionID:      agent.ToolSessionID(ctx),
			stdin:          bytes.NewReader(arguments),
			separateStderr: true,
			supervised:     background || declaration.Detach,
			detach:         declaration.Detach,
			environment:    declaration.Env,
			prepare: func(policy *filesystemPolicy) (string, error) {
				workdir, display, info, err := policy.workspaceOnly().resolveExistingPath(declaration.Workdir, true)
				if err != nil {
					return "", err
				}
				if !info.IsDir() {
					return "", fmt.Errorf("workdir %s is not a directory", display)
				}
				return workdir, nil
			},
			build: func(policy *filesystemPolicy, workdir string) (*exec.Cmd, error) {
				if policy.protection.contains(program) {
					return nil, errors.New("program is protected")
				}
				return sandboxedProgramCommand(program, declaration.Command, workdir, policy.processBoundary(manager.toolHome), declaration.Env)
			},
		})
		if err != nil {
			return agent.ToolOutput{}, fmt.Errorf("%w: %s: %v", agent.ErrToolFatal, declaration.Name, err)
		}
		if background {
			return manager.result(job, jobResultOptions{managed: true})
		}
		var yielded <-chan time.Time
		var timer *time.Timer
		if declaration.Yield > 0 {
			timer = time.NewTimer(time.Duration(declaration.Yield) * time.Second)
			defer timer.Stop()
			yielded = timer.C
		}
		select {
		case <-job.done:
			return manager.completedProgramResult(job, declaration.Name)
		case <-yielded:
			job.mu.Lock()
			job.joinRequired = true
			job.mu.Unlock()
			select {
			case <-job.done:
				return manager.completedProgramResult(job, declaration.Name)
			default:
			}
			output, truncated := manager.jobOutput(job, defaultCommandPreview)
			return manager.result(job, jobResultOptions{output: output, includeOutput: true, managed: true, truncated: truncated})
		case <-ctx.Done():
			_, _ = manager.stop(context.Background(), job.id, "tool call cancelled")
			manager.forget(job)
			return agent.ToolOutput{}, ctx.Err()
		}
	}
}

func (manager *ProcessManager) completedProgramResult(job *processJob, name string) (agent.ToolOutput, error) {
	launchFailure := manager.programLaunchFailure(job)
	content, truncated := manager.jobOutput(job, maxJobReadBytes)
	if !job.snapshot().supervised {
		manager.forget(job)
	}
	output, err := manager.result(job, jobResultOptions{output: content, includeOutput: true, truncated: truncated})
	if launchFailure != "" {
		err = errors.Join(err, fmt.Errorf("%w: %s: %s", agent.ErrToolFatal, name, launchFailure))
	}
	return output, err
}

func (manager *ProcessManager) programLaunchFailure(job *processJob) string {
	state := job.snapshot()
	if state.status == ProcessNotStarted {
		return strings.TrimSpace(state.errText)
	}
	if state.scope == "" || state.exitCode == nil || *state.exitCode != 126 {
		return ""
	}
	var stderr []byte
	if state.supervised {
		stderr, _ = readDurableTail(filepath.Join(job.jobDir, jobStderrFile), processFailureTailSize)
	} else if job.errLog != nil {
		stderr, _ = job.errLog.snapshot(processFailureTailSize)
	} else {
		return ""
	}
	message := strings.TrimSpace(string(stderr))
	for _, prefix := range []string{
		"filesystem boundary:", "filesystem boundary exec ",
		"sandbox-exec:",
	} {
		if strings.HasPrefix(message, prefix) {
			return message
		}
	}
	return ""
}
