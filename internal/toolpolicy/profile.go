// Package toolpolicy contains Skot's user-facing tool profiles. Profiles are
// application policy: the agent runtime only receives the concrete tool set
// selected for a run.
package toolpolicy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/levmv/skot/agent"
)

const (
	ProfileFull     = "full"
	ProfileEdit     = "edit"
	ProfileReadOnly = "read-only"
)

var defaultProfileOrder = []string{ProfileReadOnly, ProfileEdit, ProfileFull}

var defaultProfileTools = map[string][]string{
	ProfileReadOnly: {"read", "ls", "grep", "glob"},
	ProfileEdit:     {"read", "ls", "grep", "glob", "edit", "write"},
	// Full deliberately omits ls: Bash provides richer directory inspection.
	ProfileFull: {"read", "grep", "glob", "edit", "write", "bash", "job"},
}

var optionalDefaultTools = []string{"web_fetch", "web_search"}

// Profiles is a validated set of exact, ordered tool-name lists. User
// definitions replace built-in profiles with the same normalized name and add
// profiles under new names.
type Profiles struct {
	definitions map[string][]string
	names       []string
}

// NewProfiles builds and validates profiles against the final tool catalog.
// Override maps are applied in order; a later definition with the same
// normalized name replaces the earlier definition completely.
func NewProfiles(catalog []agent.Tool, overrides ...map[string][]string) (Profiles, error) {
	available := make(map[string]agent.Tool, len(catalog))
	for _, tool := range catalog {
		name := strings.TrimSpace(tool.Spec.Name)
		if name == "" {
			return Profiles{}, fmt.Errorf("tool name is required")
		}
		if _, exists := available[name]; exists {
			return Profiles{}, fmt.Errorf("duplicate tool %q", name)
		}
		available[name] = tool
	}

	definitions := make(map[string][]string, len(defaultProfileTools))
	for name, tools := range defaultProfileTools {
		definitions[name] = append([]string(nil), tools...)
		for _, optional := range optionalDefaultTools {
			if _, exists := available[optional]; exists {
				definitions[name] = append(definitions[name], optional)
			}
		}
	}
	for _, configured := range overrides {
		seen := make(map[string]string, len(configured))
		for rawName, tools := range configured {
			if strings.TrimSpace(rawName) == "" {
				return Profiles{}, fmt.Errorf("profile name is required")
			}
			name, err := normalizeName(rawName)
			if err != nil {
				return Profiles{}, err
			}
			if previous, exists := seen[name]; exists {
				return Profiles{}, fmt.Errorf("profile names %q and %q normalize to the same name %q", previous, rawName, name)
			}
			seen[name] = rawName
			definitions[name] = append([]string(nil), tools...)
		}
	}

	for name, tools := range definitions {
		normalized, err := normalizeToolNames(name, tools, available)
		if err != nil {
			return Profiles{}, err
		}
		definitions[name] = normalized
	}
	return Profiles{definitions: definitions, names: orderedNames(definitions)}, nil
}

// Normalize returns the canonical name of an available profile.
func (profiles Profiles) Normalize(value string) (string, error) {
	name, err := normalizeName(value)
	if err != nil {
		return "", err
	}
	if _, exists := profiles.definitions[name]; !exists {
		return "", fmt.Errorf("unknown profile %q (want %s)", name, strings.Join(profiles.names, ", "))
	}
	return name, nil
}

// Names returns profile names in stable picker order.
func (profiles Profiles) Names() []string {
	return append([]string(nil), profiles.names...)
}

// Tools returns a profile's exact ordered tool list. The catalog may be
// rebuilt after Profiles was created, so mismatches are reported at this
// boundary instead of leaking a zero-value tool into the runtime.
func (profiles Profiles) Tools(catalog []agent.Tool, profile string) ([]agent.Tool, error) {
	names, exists := profiles.definitions[profile]
	if !exists {
		return nil, fmt.Errorf("unknown profile %q", profile)
	}
	byName := make(map[string]agent.Tool, len(catalog))
	for _, tool := range catalog {
		byName[tool.Spec.Name] = tool
	}
	selected := make([]agent.Tool, 0, len(names))
	for _, name := range names {
		tool, exists := byName[name]
		if !exists {
			return nil, fmt.Errorf("profile %q requires tool %q, which is absent from the catalog", profile, name)
		}
		selected = append(selected, tool)
	}
	return selected, nil
}

// RequireTogether rejects profiles which select any dependent tool without
// also selecting required. It preserves exact profile semantics: dependencies
// are reported, never silently appended.
func (profiles Profiles) RequireTogether(required string, dependents map[string]struct{}) error {
	if len(dependents) == 0 {
		return nil
	}
	for profile, names := range profiles.definitions {
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
			return fmt.Errorf("profile %q selects background-capable tool %q but not required tool %q", profile, selectedDependent, required)
		}
	}
	return nil
}

func normalizeName(value string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(value))
	if name == "" {
		return ProfileFull, nil
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return "", fmt.Errorf("profile name %q cannot contain whitespace", value)
	}
	return name, nil
}

func normalizeToolNames(profile string, input []string, available map[string]agent.Tool) ([]string, error) {
	normalized := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, value := range input {
		name := strings.TrimSpace(value)
		if name == "" {
			return nil, fmt.Errorf("profile %q contains an empty tool name", profile)
		}
		if _, exists := available[name]; !exists {
			return nil, fmt.Errorf("profile %q refers to unknown tool %q", profile, name)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("profile %q contains duplicate tool %q", profile, name)
		}
		seen[name] = struct{}{}
		normalized = append(normalized, name)
	}
	return normalized, nil
}

func orderedNames(definitions map[string][]string) []string {
	names := make([]string, 0, len(definitions))
	seen := make(map[string]struct{}, len(defaultProfileOrder))
	for _, name := range defaultProfileOrder {
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
