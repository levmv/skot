package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/state"
	"github.com/levmv/skot/internal/toolpolicy"
	workspacetools "github.com/levmv/skot/tools"
)

type builtToolCatalog struct {
	tools            []agent.Tool
	programSnapshots []agent.ProgramToolSnapshot
	toolSets         toolpolicy.ToolSets
}

func buildToolCatalog(config Config, settings state.Settings, credentials *state.Store, masker *secretMasker, base []agent.Tool, processes *workspacetools.ProcessManager, builtInOptions toolpolicy.BuiltInOptions) (builtToolCatalog, error) {
	catalog := append([]agent.Tool(nil), base...)
	catalog = append(catalog, processes.Tools()...)
	catalog = append(catalog, workspacetools.NewWebTools(webCredentialLookup(credentials))...)
	var err error
	if config.ConfigureTools != nil {
		catalog, err = config.ConfigureTools(catalog)
		if err != nil {
			return builtToolCatalog{}, fmt.Errorf("configure tools: %w", err)
		}
	}
	catalog, err = agent.NormalizeTools(catalog)
	if err != nil {
		return builtToolCatalog{}, fmt.Errorf("validate tool catalog: %w", err)
	}

	toolsFile := strings.TrimSpace(config.ToolsFile)
	if toolsFile == "" {
		toolsFile = filepath.Join(config.Home, "tools.json")
	}
	programConfig, err := workspacetools.LoadProgramTools(toolsFile)
	if err != nil {
		return builtToolCatalog{}, err
	}
	for _, declaration := range programConfig.Tools {
		for _, value := range declaration.Env {
			masker.Add(value)
		}
	}
	resolvedPrograms, err := processes.ResolveProgramTools(programConfig.Tools)
	if err != nil {
		return builtToolCatalog{}, fmt.Errorf("configure program tools from %s: %w", toolsFile, err)
	}

	knownNames := make(map[string]struct{}, len(catalog)+len(resolvedPrograms))
	for _, tool := range catalog {
		knownNames[tool.Spec.Name] = struct{}{}
	}
	programSnapshots := make([]agent.ProgramToolSnapshot, 0, len(resolvedPrograms))
	// A foreground Bash command can yield a managed job even when background
	// was not requested, so every tool set containing bash also needs job.
	backgroundCapable := map[string]struct{}{"bash": {}}
	for _, resolved := range resolvedPrograms {
		name := resolved.Tool.Spec.Name
		if _, exists := knownNames[name]; exists {
			return builtToolCatalog{}, fmt.Errorf("tool %q in %s is already declared; give it another name", name, toolsFile)
		}
		knownNames[name] = struct{}{}
		catalog = append(catalog, resolved.Tool)
		programSnapshots = append(programSnapshots, resolved.Snapshot)
		if resolved.CanBackground {
			backgroundCapable[name] = struct{}{}
		}
	}
	catalog, err = agent.NormalizeTools(catalog)
	if err != nil {
		return builtToolCatalog{}, fmt.Errorf("validate complete tool catalog: %w", err)
	}
	toolSets, err := toolpolicy.NewToolSetsWithOptions(catalog, builtInOptions, settings.ToolSets, config.ToolSets)
	if err != nil {
		return builtToolCatalog{}, fmt.Errorf("configure tool sets: %w", err)
	}
	if err := toolSets.RequireTogether("job", backgroundCapable); err != nil {
		return builtToolCatalog{}, err
	}
	return builtToolCatalog{
		tools:            catalog,
		programSnapshots: programSnapshots,
		toolSets:         toolSets,
	}, nil
}

func toolSetTools(toolSets toolpolicy.ToolSets, tools []agent.Tool, credentials *state.Store, toolSet string) ([]agent.Tool, error) {
	selected, err := toolSets.Tools(tools, toolSet)
	if err != nil {
		return nil, err
	}
	searchAvailable, err := workspacetools.WebSearchAvailable(webCredentialLookup(credentials))
	if err != nil {
		return nil, fmt.Errorf("check web search credentials: %w", err)
	}
	if searchAvailable {
		return selected, nil
	}
	filtered := selected[:0]
	for _, tool := range selected {
		if tool.Spec.Name != "web_search" {
			filtered = append(filtered, tool)
		}
	}
	return filtered, nil
}

func toolSetNeedsProcessBoundary(toolSets toolpolicy.ToolSets, programs []agent.ProgramToolSnapshot, toolSet string) bool {
	processTools := make(map[string]struct{}, len(programs)+1)
	processTools["bash"] = struct{}{}
	for _, program := range programs {
		processTools[program.Name] = struct{}{}
	}
	for _, name := range toolSets.ToolNames(toolSet) {
		if _, exists := processTools[name]; exists {
			return true
		}
	}
	return false
}

func webCredentialLookup(credentials *state.Store) workspacetools.WebCredentialLookup {
	return func(provider string) (string, error) {
		token, _, err := credentialForProvider(credentials, provider)
		return token, err
	}
}
