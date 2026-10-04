package modelconfig

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"

	"github.com/levmv/skot/internal/codexauth"
	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/internal/state"
)

var ErrInvalidCodexCredentials = errors.New("invalid ChatGPT credentials; retry /login openai-codex")

func codexTokens(profile state.CredentialProfile) (codexauth.Tokens, error) {
	if profile.Kind != "oauth" || profile.Provider != codexauth.Provider {
		return codexauth.Tokens{}, nil
	}
	var tokens codexauth.Tokens
	if err := json.Unmarshal(profile.Payload, &tokens); err != nil || !tokens.Valid() {
		return codexauth.Tokens{}, ErrInvalidCodexCredentials
	}
	return tokens, nil
}

func StoredCodexTokens(store CredentialStore) (codexauth.Tokens, error) {
	if store == nil {
		return codexauth.Tokens{}, nil
	}
	profile, err := store.Credential(codexauth.Provider)
	if err != nil {
		return codexauth.Tokens{}, err
	}
	return codexTokens(profile)
}

func CodexProfile(tokens codexauth.Tokens) (state.CredentialProfile, error) {
	payload, err := json.Marshal(tokens, json.Deterministic(true))
	return state.CredentialProfile{Provider: codexauth.Provider, Kind: "oauth", Payload: payload}, err
}

func MaskCodexTokens(masker interface{ Add(string) }, tokens codexauth.Tokens) {
	if masker == nil {
		return
	}
	masker.Add(tokens.AccessToken)
	masker.Add(tokens.RefreshToken)
}

type CodexAuthorizer struct {
	Store    CredentialStore
	ModelURI string
	Client   *http.Client
	Masker   interface{ Add(string) }
}

func (authorizer CodexAuthorizer) Authorize(ctx context.Context, request *http.Request) error {
	if request.URL.Scheme != "https" || request.URL.Host != "chatgpt.com" || request.URL.Path != "/backend-api/codex/responses" || request.URL.User != nil {
		return errors.New("ChatGPT credentials can only be used with the Codex endpoint")
	}
	tokens, err := authorizer.CurrentTokens(ctx)
	if err != nil {
		return err
	}
	SetCodexAccountHeaders(request, tokens)
	request.Header.Set("OpenAI-Beta", "responses=experimental")
	if sessionID := request.Header.Get("X-Session-ID"); sessionID != "" {
		request.Header.Set("session-id", sessionID)
	}
	return nil
}

func (authorizer CodexAuthorizer) CurrentTokens(ctx context.Context) (codexauth.Tokens, error) {
	tokens, err := StoredCodexTokens(authorizer.Store)
	if err != nil {
		return codexauth.Tokens{}, err
	}
	if !tokens.Valid() {
		return codexauth.Tokens{}, missingProviderCredentialError(codexauth.Provider, authorizer.ModelURI)
	}
	MaskCodexTokens(authorizer.Masker, tokens)
	if tokens.NeedsRefresh() {
		err := authorizer.Store.UpdateCredential(ctx, codexauth.Provider, func(profile state.CredentialProfile) (state.CredentialProfile, error) {
			// A different Skot process may already have refreshed or logged out.
			current, err := codexTokens(profile)
			if err != nil {
				return profile, err
			}
			if !current.Valid() {
				return profile, missingProviderCredentialError(codexauth.Provider, authorizer.ModelURI)
			}
			MaskCodexTokens(authorizer.Masker, current)
			if current.NeedsRefresh() {
				current, err = codexauth.Refresh(ctx, authorizer.Client, current.RefreshToken)
				if err != nil {
					return profile, err
				}
				MaskCodexTokens(authorizer.Masker, current)
				profile, err = CodexProfile(current)
			}
			tokens = current
			return profile, err
		})
		if err != nil {
			return codexauth.Tokens{}, err
		}
	}
	return tokens, nil
}

func SetCodexAccountHeaders(request *http.Request, tokens codexauth.Tokens) {
	request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	request.Header.Set("ChatGPT-Account-Id", tokens.AccountID)
	if tokens.Residency != "" {
		request.Header.Set("x-openai-internal-codex-residency", tokens.Residency)
	}
	request.Header.Set("originator", "skot")
}

func CodexHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = modelhttp.DefaultClient()
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}
