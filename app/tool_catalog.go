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
	tools               []agent.Tool
	programDeclarations []workspacetools.ProgramTool
	programToolsFile    string
	toolSets            toolpolicy.ToolSets
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
	describedPrograms, err := workspacetools.DescribeProgramTools(programConfig.Tools)
	if err != nil {
		return builtToolCatalog{}, fmt.Errorf("describe program tools from %s: %w", toolsFile, err)
	}

	knownNames := make(map[string]struct{}, len(catalog)+len(describedPrograms))
	for _, tool := range catalog {
		knownNames[tool.Spec.Name] = struct{}{}
	}
	// A foreground Bash command can yield a managed job even when background
	// was not requested, so every tool set containing bash also needs job.
	backgroundCapable := map[string]struct{}{"bash": {}}
	for _, described := range describedPrograms {
		name := described.Tool.Spec.Name
		if _, exists := knownNames[name]; exists {
			return builtToolCatalog{}, fmt.Errorf("tool %q in %s is already declared; give it another name", name, toolsFile)
		}
		knownNames[name] = struct{}{}
		catalog = append(catalog, described.Tool)
		if described.CanBackground {
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
		tools:               catalog,
		programDeclarations: append([]workspacetools.ProgramTool(nil), programConfig.Tools...),
		programToolsFile:    toolsFile,
		toolSets:            toolSets,
	}, nil
}

func bindProgramToolsForSet(catalog []agent.Tool, toolSets toolpolicy.ToolSets, toolSet string, declarations []workspacetools.ProgramTool, toolsFile string, processes *workspacetools.ProcessManager) ([]agent.Tool, []agent.ProgramToolSnapshot, error) {
	selectedNames := make(map[string]struct{}, len(toolSets.ToolNames(toolSet)))
	for _, name := range toolSets.ToolNames(toolSet) {
		selectedNames[name] = struct{}{}
	}
	selectedDeclarations := make([]workspacetools.ProgramTool, 0, len(declarations))
	for _, declaration := range declarations {
		if _, selected := selectedNames[declaration.Name]; selected {
			selectedDeclarations = append(selectedDeclarations, declaration)
		}
	}
	if len(selectedDeclarations) == 0 {
		return append([]agent.Tool(nil), catalog...), nil, nil
	}
	if processes == nil {
		return nil, nil, fmt.Errorf("configure selected program tools from %s: process manager is unavailable", toolsFile)
	}
	resolved, err := processes.ResolveProgramTools(selectedDeclarations)
	if err != nil {
		return nil, nil, fmt.Errorf("configure selected program tools from %s: %w", toolsFile, err)
	}
	replacements := make(map[string]agent.Tool, len(resolved))
	snapshots := make([]agent.ProgramToolSnapshot, 0, len(resolved))
	for _, program := range resolved {
		replacements[program.Tool.Spec.Name] = program.Tool
		snapshots = append(snapshots, program.Snapshot)
	}
	bound := append([]agent.Tool(nil), catalog...)
	for index, tool := range bound {
		if replacement, exists := replacements[tool.Spec.Name]; exists {
			bound[index] = replacement
		}
	}
	return bound, snapshots, nil
}

func bindToolSetTools(catalog []agent.Tool, toolSets toolpolicy.ToolSets, credentials *state.Store, toolSet string, declarations []workspacetools.ProgramTool, toolsFile string, processes *workspacetools.ProcessManager) ([]agent.Tool, []agent.ProgramToolSnapshot, error) {
	bound, snapshots, err := bindProgramToolsForSet(catalog, toolSets, toolSet, declarations, toolsFile, processes)
	if err != nil {
		return nil, nil, err
	}
	selected, err := toolSetTools(toolSets, bound, credentials, toolSet)
	if err != nil {
		return nil, nil, err
	}
	return selected, snapshots, nil
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

func toolSetNeedsProcessBoundary(toolSets toolpolicy.ToolSets, programs []workspacetools.ProgramTool, toolSet string) bool {
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

func freshHeadlessMemorySession(config Config, toolSets toolpolicy.ToolSets) bool {
	if config.Interactive || config.SaveSession || config.Resume || strings.TrimSpace(config.JournalPath) != "" {
		return false
	}
	// ConfigureTools can replace even a familiar safe name with an
	// application-owned tool whose external-work semantics Skot cannot infer.
	if config.ConfigureTools != nil {
		return false
	}
	return toolSetSupportsMemorySession(toolSets, config.ToolSet)
}

func toolSetSupportsMemorySession(toolSets toolpolicy.ToolSets, toolSet string) bool {
	safeBuiltIns := map[string]struct{}{
		"read": {}, "ls": {}, "grep": {}, "glob": {}, "edit": {}, "write": {},
		"web_fetch": {}, "web_search": {},
	}
	for _, name := range toolSets.ToolNames(toolSet) {
		if _, safe := safeBuiltIns[name]; safe {
			continue
		}
		return false
	}
	return true
}

func webCredentialLookup(credentials *state.Store) workspacetools.WebCredentialLookup {
	return func(provider string) (string, error) {
		token, _, err := credentialForProvider(credentials, provider)
		return token, err
	}
}
