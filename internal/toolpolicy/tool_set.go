// Package toolpolicy contains Skot's user-facing named tool sets. Tool sets are
// application policy: the agent runtime only receives the concrete tools
// selected for a run.
package toolpolicy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/levmv/skot/agent"
)

const (
	ToolSetDefault  = "default"
	ToolSetEdit     = "edit"
	ToolSetReadOnly = "read-only"
)

// The default set leads: it is what a session runs unless told otherwise.
var builtInToolSetOrder = []string{ToolSetDefault, ToolSetEdit, ToolSetReadOnly}

var builtInToolSetTools = map[string][]string{
	ToolSetReadOnly: {"read", "ls", "grep", "glob"},
	ToolSetEdit:     {"read", "ls", "grep", "glob", "edit", "write"},
	// Default normally omits ls: Bash provides richer directory inspection.
	ToolSetDefault: {"read", "grep", "glob", "edit", "write", "bash", "job"},
}

var optionalBuiltInTools = []string{"web_fetch", "web_search"}

// BuiltInOptions adjusts product-owned sets before exact user overrides are
// applied.
type BuiltInOptions struct {
	// DefaultIncludesLS covers layouts where process sandboxing can prevent
	// Bash from enumerating the workspace root.
	DefaultIncludesLS bool
}

// ToolSets is a validated collection of exact, ordered tool-name lists. User
// definitions replace built-in sets with the same normalized name and add sets
// under new names.
type ToolSets struct {
	definitions map[string][]string
	names       []string
}

// NewToolSets builds and validates named sets against the final tool catalog.
// Override maps are applied in order; a later definition with the same
// normalized name replaces the earlier definition completely.
func NewToolSets(catalog []agent.Tool, overrides ...map[string][]string) (ToolSets, error) {
	return NewToolSetsWithOptions(catalog, BuiltInOptions{}, overrides...)
}

// NewToolSetsWithOptions builds named sets with conditional product-owned
// defaults. User definitions still replace a built-in set exactly.
func NewToolSetsWithOptions(catalog []agent.Tool, options BuiltInOptions, overrides ...map[string][]string) (ToolSets, error) {
	available := make(map[string]agent.Tool, len(catalog))
	for _, tool := range catalog {
		name := strings.TrimSpace(tool.Spec.Name)
		if name == "" {
			return ToolSets{}, fmt.Errorf("tool name is required")
		}
		if _, exists := available[name]; exists {
			return ToolSets{}, fmt.Errorf("duplicate tool %q", name)
		}
		available[name] = tool
	}

	definitions := make(map[string][]string, len(builtInToolSetTools))
	for name, tools := range builtInToolSetTools {
		definitions[name] = append([]string(nil), tools...)
		if name == ToolSetDefault && options.DefaultIncludesLS {
			withLS := make([]string, 0, len(definitions[name])+1)
			withLS = append(withLS, definitions[name][0], "ls")
			definitions[name] = append(withLS, definitions[name][1:]...)
		}
		for _, optional := range optionalBuiltInTools {
			if _, exists := available[optional]; exists {
				definitions[name] = append(definitions[name], optional)
			}
		}
	}
	for _, configured := range overrides {
		seen := make(map[string]string, len(configured))
		for rawName, tools := range configured {
			if strings.TrimSpace(rawName) == "" {
				return ToolSets{}, fmt.Errorf("tool set name is required")
			}
			name, err := normalizeName(rawName)
			if err != nil {
				return ToolSets{}, err
			}
			if previous, exists := seen[name]; exists {
				return ToolSets{}, fmt.Errorf("tool set names %q and %q normalize to the same name %q", previous, rawName, name)
			}
			seen[name] = rawName
			definitions[name] = append([]string(nil), tools...)
		}
	}

	for name, tools := range definitions {
		normalized, err := normalizeToolNames(name, tools, available)
		if err != nil {
			return ToolSets{}, err
		}
		definitions[name] = normalized
	}
	return ToolSets{definitions: definitions, names: orderedNames(definitions)}, nil
}

// Normalize returns the canonical name of an available tool set.
func (toolSets ToolSets) Normalize(value string) (string, error) {
	name, err := normalizeName(value)
	if err != nil {
		return "", err
	}
	if _, exists := toolSets.definitions[name]; !exists {
		return "", fmt.Errorf("unknown tool set %q (want %s)", name, strings.Join(toolSets.names, ", "))
	}
	return name, nil
}

// Names returns tool set names in stable picker order.
func (toolSets ToolSets) Names() []string {
	return append([]string(nil), toolSets.names...)
}

// ToolNames returns an owned copy of a set's exact configured tool names.
// Unknown sets return nil.
func (toolSets ToolSets) ToolNames(toolSet string) []string {
	return append([]string(nil), toolSets.definitions[toolSet]...)
}

// Tools returns a set's exact ordered tool list. The catalog may be rebuilt
// after ToolSets was created, so mismatches are reported at this boundary
// instead of leaking a zero-value tool into the runtime.
func (toolSets ToolSets) Tools(catalog []agent.Tool, toolSet string) ([]agent.Tool, error) {
	names, exists := toolSets.definitions[toolSet]
	if !exists {
		return nil, fmt.Errorf("unknown tool set %q", toolSet)
	}
	byName := make(map[string]agent.Tool, len(catalog))
	for _, tool := range catalog {
		byName[tool.Spec.Name] = tool
	}
	selected := make([]agent.Tool, 0, len(names))
	for _, name := range names {
		tool, exists := byName[name]
		if !exists {
			return nil, fmt.Errorf("tool set %q requires tool %q, which is absent from the catalog", toolSet, name)
		}
		selected = append(selected, tool)
	}
	return selected, nil
}

// RequireTogether rejects sets which select any dependent tool without also
// selecting required. It preserves exact set semantics: dependencies are
// reported, never silently appended.
func (toolSets ToolSets) RequireTogether(required string, dependents map[string]struct{}) error {
	if len(dependents) == 0 {
		return nil
	}
	for toolSet, names := range toolSets.definitions {
		hasRequired := false
		var selectedDependent string
		for _, name := range names {
			if name == required {
				hasRequired = true
			}
			if _, exists := dependents[name]; exists && selectedDependent == "" {
				selectedDependent = name
			}
		}
		if selectedDependent != "" && !hasRequired {
			return fmt.Errorf("tool set %q selects background-capable tool %q but not required tool %q", toolSet, selectedDependent, required)
		}
	}
	return nil
}

func normalizeName(value string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(value))
	if name == "" {
		return ToolSetDefault, nil
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return "", fmt.Errorf("tool set name %q cannot contain whitespace", value)
	}
	return name, nil
}

func normalizeToolNames(toolSet string, input []string, available map[string]agent.Tool) ([]string, error) {
	normalized := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, value := range input {
		name := strings.TrimSpace(value)
		if name == "" {
			return nil, fmt.Errorf("tool set %q contains an empty tool name", toolSet)
		}
		if _, exists := available[name]; !exists {
			return nil, fmt.Errorf("tool set %q refers to unknown tool %q", toolSet, name)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("tool set %q contains duplicate tool %q", toolSet, name)
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	return normalized, nil
}

func orderedNames(definitions map[string][]string) []string {
	names := make([]string, 0, len(definitions))
	seen := make(map[string]struct{}, len(builtInToolSetOrder))
	for _, name := range builtInToolSetOrder {
		if _, exists := definitions[name]; exists {
			names = append(names, name)
			seen[name] = struct{}{}
		}
	}
	var custom []string
	for name := range definitions {
		if _, exists := seen[name]; !exists {
			custom = append(custom, name)
		}
	}
	sort.Strings(custom)
	return append(names, custom...)
}
