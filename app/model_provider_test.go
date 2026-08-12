package app

import "testing"

func TestParseModelURIPreservesSlashInModel(t *testing.T) {
	provider, model, err := parseModelURI("openrouter/moonshotai/kimi-k3")
	if err != nil {
		t.Fatal(err)
	}
	if provider != "openrouter" || model != "moonshotai/kimi-k3" {
		t.Fatalf("provider/model = %q/%q", provider, model)
	}
}

func TestOllamaProviderUsesLocalOpenAICompatibilityEndpoint(t *testing.T) {
	spec, err := modelProviderSpec(" OLLAMA ")
	if err != nil {
		t.Fatal(err)
	}
	if spec.baseURL != "http://localhost:11434/v1" || !spec.credentialless {
		t.Fatalf("Ollama provider = %#v", spec)
	}
}
