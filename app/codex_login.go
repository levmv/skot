package app

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/levmv/skot/internal/codexauth"
	"github.com/levmv/skot/internal/modelhttp"
	"github.com/levmv/skot/internal/state"
)

// BrowserLogin stages a browser authorization without changing stored
// credentials. Call Wait once, then Complete after success to save the login.
// SubmitRedirect is safe while Wait is running. Close cancels outstanding work
// and must also be called when a completed login is abandoned.
type BrowserLogin interface {
	AuthorizationURL() string
	SubmitRedirect(string) error
	Wait() error
	Complete(context.Context) error
	Close()
}

type codexBrowserLogin struct {
	flow        *codexauth.Login
	ctx         context.Context
	cancel      context.CancelFunc
	application *Application
	tokens      codexauth.Tokens
}

var errInvalidCodexCredentials = errors.New("invalid ChatGPT credentials; retry /login openai-codex")

func (application *Application) BeginBrowserLogin(ctx context.Context, provider string) (BrowserLogin, error) {
	if _, err := application.requireRuntime(); err != nil {
		return nil, err
	}
	if strings.ToLower(strings.TrimSpace(provider)) != codexauth.Provider {
		return nil, fmt.Errorf("%s does not support browser login", provider)
	}
	if application.config.settings == nil {
		return nil, errors.New("auth store is unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	flow, err := codexauth.Start(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	return &codexBrowserLogin{flow: flow, ctx: ctx, cancel: cancel, application: application}, nil
}

func (login *codexBrowserLogin) AuthorizationURL() string { return login.flow.URL }
func (login *codexBrowserLogin) SubmitRedirect(value string) error {
	return login.flow.SubmitRedirect(value)
}
func (login *codexBrowserLogin) Close() {
	login.cancel()
	login.flow.Close()
}

func (login *codexBrowserLogin) Wait() error {
	tokens, err := login.flow.Wait()
	if err != nil {
		return err
	}
	login.tokens = tokens
	maskCodexTokens(login.application.config.masker, tokens)
	return nil
}

func (login *codexBrowserLogin) Complete(ctx context.Context) error {
	if err := login.ctx.Err(); err != nil {
		return err
	}
	if !login.tokens.Valid() {
		return errors.New("browser login has not completed")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(login.ctx, cancel)
	defer stop()
	profile, err := codexProfile(login.tokens)
	if err != nil {
		return err
	}
	return login.application.updateCredential(ctx, codexauth.Provider, func(store *state.Store, provider string) error {
		return store.UpdateCredential(ctx, provider, func(state.CredentialProfile) (state.CredentialProfile, error) {
			if err := ctx.Err(); err != nil {
				return state.CredentialProfile{}, err
			}
			return profile, nil
		})
	})
}

func codexTokens(profile state.CredentialProfile) (codexauth.Tokens, error) {
	if profile.Kind != "oauth" || profile.Provider != codexauth.Provider {
		return codexauth.Tokens{}, nil
	}
	var tokens codexauth.Tokens
	if err := json.Unmarshal(profile.Payload, &tokens); err != nil || !tokens.Valid() {
		return codexauth.Tokens{}, errInvalidCodexCredentials
	}
	return tokens, nil
}

func storedCodexTokens(store *state.Store) (codexauth.Tokens, error) {
	if store == nil {
		return codexauth.Tokens{}, nil
	}
	profile, err := store.Credential(codexauth.Provider)
	if err != nil {
		return codexauth.Tokens{}, err
	}
	return codexTokens(profile)
}

func codexProfile(tokens codexauth.Tokens) (state.CredentialProfile, error) {
	payload, err := json.Marshal(tokens, json.Deterministic(true))
	return state.CredentialProfile{Provider: codexauth.Provider, Kind: "oauth", Payload: payload}, err
}

func maskCodexTokens(masker *secretMasker, tokens codexauth.Tokens) {
	masker.Add(tokens.AccessToken)
	masker.Add(tokens.RefreshToken)
}

type codexAuthorizer struct {
	store    *state.Store
	modelURI string
	client   *http.Client
	masker   *secretMasker
}

func (authorizer codexAuthorizer) Authorize(ctx context.Context, request *http.Request) error {
	if request.URL.Scheme != "https" || request.URL.Host != "chatgpt.com" || request.URL.Path != "/backend-api/codex/responses" || request.URL.User != nil {
		return errors.New("ChatGPT credentials can only be used with the Codex endpoint")
	}
	tokens, err := authorizer.currentTokens(ctx)
	if err != nil {
		return err
	}
	setCodexAccountHeaders(request, tokens)
	request.Header.Set("OpenAI-Beta", "responses=experimental")
	if sessionID := request.Header.Get("X-Session-ID"); sessionID != "" {
		request.Header.Set("session-id", sessionID)
	}
	return nil
}

func (authorizer codexAuthorizer) currentTokens(ctx context.Context) (codexauth.Tokens, error) {
	tokens, err := storedCodexTokens(authorizer.store)
	if err != nil {
		return codexauth.Tokens{}, err
	}
	if !tokens.Valid() {
		return codexauth.Tokens{}, missingProviderCredentialError(codexauth.Provider, authorizer.modelURI)
	}
	maskCodexTokens(authorizer.masker, tokens)
	if tokens.NeedsRefresh() {
		err := authorizer.store.UpdateCredential(ctx, codexauth.Provider, func(profile state.CredentialProfile) (state.CredentialProfile, error) {
			// A different Skot process may already have refreshed or logged out.
			current, err := codexTokens(profile)
			if err != nil {
				return profile, err
			}
			if !current.Valid() {
				return profile, missingProviderCredentialError(codexauth.Provider, authorizer.modelURI)
			}
			maskCodexTokens(authorizer.masker, current)
			if current.NeedsRefresh() {
				current, err = codexauth.Refresh(ctx, authorizer.client, current.RefreshToken)
				if err != nil {
					return profile, err
				}
				maskCodexTokens(authorizer.masker, current)
				profile, err = codexProfile(current)
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

func setCodexAccountHeaders(request *http.Request, tokens codexauth.Tokens) {
	request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	request.Header.Set("ChatGPT-Account-Id", tokens.AccountID)
	if tokens.Residency != "" {
		request.Header.Set("x-openai-internal-codex-residency", tokens.Residency)
	}
	request.Header.Set("originator", "skot")
}

func codexHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = modelhttp.DefaultClient()
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}
