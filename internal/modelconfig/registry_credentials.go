package modelconfig

import (
	"slices"
	"sort"
	"strings"
)

func (registry *Registry) ProviderEnvironment(provider string) string {
	if definition, _ := registry.definition(strings.ToLower(strings.TrimSpace(provider))); definition.APIKeyEnv != nil {
		return *definition.APIKeyEnv
	}
	return ProviderEnvironment(provider)
}

func (registry *Registry) CredentialSpecs() []CredentialSpec {
	specs := slices.Clone(CredentialCatalog)
	seen := make(map[string]bool)
	for i := range specs {
		specs[i].Environment = registry.ProviderEnvironment(specs[i].Name)
		if provider, err := registry.Provider(specs[i].Name); err == nil && provider.CustomEndpoint {
			specs[i].CredentialURL = ""
		}
		seen[specs[i].Name] = true
	}
	if registry != nil {
		var extra []string
		for name := range registry.providers {
			if !seen[name] {
				extra = append(extra, name)
			}
		}
		sort.Strings(extra)
		for _, name := range extra {
			specs = append(specs, CredentialSpec{Name: name, Environment: registry.ProviderEnvironment(name), Description: "model provider", Capabilities: CredentialModel})
		}
	}
	return specs
}

func (registry *Registry) KnownCredentialProvider(name string) bool {
	_, configured := registry.definition(strings.ToLower(strings.TrimSpace(name)))
	return configured || KnownCredentialProvider(name)
}

func (registry *Registry) CredentialEnvironmentNames() []string {
	names := CredentialEnvironmentNames()
	for _, spec := range registry.CredentialSpecs() {
		if spec.Environment != "" && !slices.Contains(names, spec.Environment) {
			names = append(names, spec.Environment)
		}
	}
	return names
}

func (registry *Registry) CredentialForProvider(store CredentialStore, name string, useEnvironment bool) (string, string, error) {
	provider, err := registry.Provider(name)
	if err == nil && provider.AuthNone {
		return "", "not required", nil
	}
	token, source, err := credentialForEnvironment(store, name, useEnvironment, registry.ProviderEnvironment(name))
	if err == nil && token == "" && provider.Credentialless {
		source = "not required"
	}
	return token, source, err
}
