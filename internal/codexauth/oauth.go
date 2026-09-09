// Package codexauth implements the ChatGPT browser login used by Codex clients.
package codexauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/modelhttp"
)

const (
	Provider     = "openai-codex"
	BaseURL      = "https://chatgpt.com/backend-api/codex"
	clientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	authorizeURL = "https://auth.openai.com/oauth/authorize"
	tokenURL     = "https://auth.openai.com/oauth/token"
	redirectURI  = "http://localhost:1455/auth/callback"
)

// Tokens are private credentials, never conversation or diagnostic data.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountID    string    `json:"account_id"`
	Residency    string    `json:"residency,omitempty"`
}

func (tokens Tokens) Valid() bool {
	return tokens.AccessToken != "" && tokens.RefreshToken != "" && !tokens.ExpiresAt.IsZero() && tokens.AccountID != ""
}

func (tokens Tokens) NeedsRefresh() bool {
	return !time.Now().Add(time.Minute).Before(tokens.ExpiresAt)
}

type callback struct {
	code string
	err  error
}

// Login owns a temporary loopback listener. Wait must be called once and Close
// must be called when the flow is finished or abandoned. A pasted full redirect
// URL works even when the browser runs on a different machine.
type Login struct {
	URL       string
	state     string
	verifier  string
	ctx       context.Context
	cancel    context.CancelFunc
	callbacks chan callback
	client    *http.Client
}

func Start(ctx context.Context) (*Login, error) {
	return start(ctx, net.Listen, nil)
}

func start(ctx context.Context, listen func(string, string) (net.Listener, error), client *http.Client) (*Login, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	login := &Login{
		state: randomString(), verifier: randomString(), ctx: ctx, cancel: cancel,
		callbacks: make(chan callback, 1), client: client,
	}
	challenge := sha256.Sum256([]byte(login.verifier))
	query := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI},
		"scope": {"openid profile email offline_access"}, "state": {login.state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
		"id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"}, "originator": {"skot"},
	}
	login.URL = authorizeURL + "?" + query.Encode()
	// Another Codex client may own the fixed redirect port. Manual input still
	// works; binding any non-loopback interface would expose the callback.
	if listener, err := listen("tcp", "127.0.0.1:1455"); err == nil {
		server := &http.Server{Handler: http.HandlerFunc(login.serveCallback), ReadHeaderTimeout: 5 * time.Second}
		stop := context.AfterFunc(ctx, func() { _ = server.Close() })
		go func() {
			defer stop()
			_ = server.Serve(listener)
		}()
	}
	return login, nil
}

func randomString() string {
	var data [32]byte
	_, _ = rand.Read(data[:])
	return base64.RawURLEncoding.EncodeToString(data[:])
}

func (login *Login) Close() { login.cancel() }

func (login *Login) SubmitRedirect(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "http" || parsed.Host != "localhost:1455" || parsed.Path != "/auth/callback" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("paste the complete http://localhost:1455/auth/callback URL from your browser")
	}
	return login.submit(parsed.Query())
}

func (login *Login) submit(query url.Values) error {
	if err := login.ctx.Err(); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(login.state)) != 1 {
		return errors.New("login state does not match; use the URL from this login attempt")
	}
	result := callback{code: query.Get("code")}
	if query.Get("error") != "" {
		result.err = errors.New("OpenAI authorization was declined; retry /login openai-codex")
	} else if result.code == "" {
		return errors.New("redirect URL has no authorization code")
	}
	select {
	case <-login.ctx.Done():
		return login.ctx.Err()
	case login.callbacks <- result:
		return nil
	default:
		return errors.New("authorization is already being completed")
	}
}

func (login *Login) serveCallback(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if request.Method != http.MethodGet || request.URL.Path != "/auth/callback" {
		http.NotFound(writer, request)
		return
	}
	if err := login.submit(request.URL.Query()); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	_, _ = io.WriteString(writer, "Return to Skot to finish signing in. You can close this window.")
}

func (login *Login) Wait() (Tokens, error) {
	defer login.Close()
	select {
	case <-login.ctx.Done():
		return Tokens{}, login.ctx.Err()
	case result := <-login.callbacks:
		if result.err != nil {
			return Tokens{}, result.err
		}
		return exchange(login.ctx, login.client, url.Values{
			"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {result.code},
			"code_verifier": {login.verifier}, "redirect_uri": {redirectURI},
		}, "")
	}
}

func Refresh(ctx context.Context, client *http.Client, refreshToken string) (Tokens, error) {
	return exchange(ctx, client, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refreshToken},
	}, refreshToken)
}

func exchange(ctx context.Context, client *http.Client, form url.Values, previousRefresh string) (Tokens, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Skot")
	if client == nil {
		client = http.DefaultClient
	}
	// Never forward the authorization code or refresh token through redirects.
	secureClient := *client
	secureClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := secureClient.Do(request)
	if err != nil {
		return Tokens{}, agent.MarkProviderFailure(fmt.Errorf("OpenAI token request: %w", err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// Token endpoint bodies may contain credentials. Only expose the status.
		if response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			return Tokens{}, modelhttp.NewProviderError(modelhttp.ProviderErrorDetails{
				Provider: Provider, StatusCode: response.StatusCode, Message: "OpenAI token request failed",
				RetryAfter: modelhttp.ParseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
			})
		}
		return Tokens{}, fmt.Errorf("OpenAI token request failed (HTTP %d); retry /login openai-codex", response.StatusCode)
	}
	var payload struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.UnmarshalRead(io.LimitReader(response.Body, 1<<20), &payload); err != nil {
		return Tokens{}, errors.New("OpenAI returned an invalid token response")
	}
	if payload.Refresh == "" {
		payload.Refresh = previousRefresh
	}
	account, residency := tokenClaims(payload.Access)
	tokens := Tokens{
		AccessToken: payload.Access, RefreshToken: payload.Refresh,
		ExpiresAt: time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second), AccountID: account, Residency: residency,
	}
	if !tokens.Valid() || payload.ExpiresIn <= 0 || payload.ExpiresIn > 365*24*60*60 {
		return Tokens{}, errors.New("OpenAI token response is missing account or token data; retry /login openai-codex")
	}
	return tokens, nil
}

// These routing hints come from the token obtained over HTTPS, not from an
// independent authentication decision. OpenAI verifies the bearer token.
func tokenClaims(token string) (account, residency string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	type authClaims struct {
		AccountID string `json:"chatgpt_account_id"`
		Residency string `json:"chatgpt_compute_residency"`
	}
	var claims struct {
		Auth authClaims `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(data, &claims); err != nil {
		return "", ""
	}
	residency = claims.Auth.Residency
	if residency == "no_constraint" {
		residency = ""
	}
	return claims.Auth.AccountID, residency
}
