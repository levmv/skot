package codexauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func testAccessToken() string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-1","chatgpt_compute_residency":"eu"}}`)) + ".signature"
}

func tokenResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func manualLogin(t *testing.T, ctx context.Context, client *http.Client) *Login {
	t.Helper()
	login, err := start(ctx, func(network, address string) (net.Listener, error) {
		if network != "tcp" || address != "127.0.0.1:1455" {
			t.Errorf("callback is not restricted to loopback: %s %s", network, address)
		}
		return nil, errors.New("port already in use")
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(login.Close)
	return login
}

func TestBrowserLoginBindsCodeToPKCEAndState(t *testing.T) {
	var authorization url.Values
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != tokenURL || request.Method != "POST" {
			t.Fatalf("token destination = %s %s", request.Method, request.URL)
		}
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		form := request.PostForm
		challenge := sha256.Sum256([]byte(form.Get("code_verifier")))
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != "private-code" || form.Get("client_id") != clientID || form.Get("redirect_uri") != redirectURI || base64.RawURLEncoding.EncodeToString(challenge[:]) != authorization.Get("code_challenge") {
			t.Fatal("code exchange did not match the browser authorization")
		}
		body, _ := json.Marshal(map[string]any{"access_token": testAccessToken(), "refresh_token": "private-refresh", "expires_in": 3600})
		return tokenResponse(string(body)), nil
	})}
	login := manualLogin(t, t.Context(), client)
	parsed, err := url.Parse(login.URL)
	if err != nil {
		t.Fatal(err)
	}
	authorization = parsed.Query()
	if parsed.Scheme != "https" || parsed.Host != "auth.openai.com" || authorization.Get("originator") != "skot" || authorization.Get("code_challenge_method") != "S256" || len(authorization.Get("state")) < 32 {
		t.Fatal("invalid browser authorization URL")
	}
	query := url.Values{"state": {"wrong-state"}, "code": {"private-code"}}
	if err := login.SubmitRedirect(redirectURI + "?" + query.Encode()); err == nil {
		t.Fatal("accepted an unrelated login state")
	}
	query.Set("state", authorization.Get("state"))
	for _, value := range []string{"private-code", "https://example.com/?" + query.Encode()} {
		if err := login.SubmitRedirect(value); err == nil {
			t.Fatal("accepted a callback without its full redirect URL")
		}
	}
	if err := login.SubmitRedirect(redirectURI + "?" + query.Encode()); err != nil {
		t.Fatal(err)
	}
	tokens, err := login.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !tokens.Valid() || tokens.NeedsRefresh() || tokens.AccountID != "account-1" || tokens.Residency != "eu" || tokens.RefreshToken != "private-refresh" {
		t.Fatal("browser login did not produce usable account credentials")
	}
}

func TestCallbackRejectsForeignStateAndReportsDeniedLogin(t *testing.T) {
	login := manualLogin(t, t.Context(), nil)
	for _, value := range []string{"/auth/callback?state=wrong&code=code", "/other?state=" + login.state} {
		writer := httptest.NewRecorder()
		login.serveCallback(writer, httptest.NewRequest("GET", value, nil))
		if writer.Code < 400 {
			t.Fatal("accepted an invalid callback")
		}
	}
	writer := httptest.NewRecorder()
	login.serveCallback(writer, httptest.NewRequest("GET", "/auth/callback?state="+login.state+"&error=access_denied&error_description=private-value", nil))
	if writer.Code != 200 || writer.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("valid callback was rejected")
	}
	if _, err := login.Wait(); err == nil || !strings.Contains(err.Error(), "declined") || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("denied login error = %v", err)
	}
}

func TestCancelledBrowserLoginClosesListener(t *testing.T) {
	var address string
	login, err := start(t.Context(), func(network, _ string) (net.Listener, error) {
		listener, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			address = listener.Addr().String()
		}
		return listener, err
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	login.Close()
	if _, err := login.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled login = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err != nil {
			break
		}
		_ = connection.Close()
		if time.Now().After(deadline) {
			t.Fatal("cancelled login left its callback listener open")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRefreshKeepsExistingTokenWhenNotRotated(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Form.Get("refresh_token") != "old-refresh" || request.Form.Get("grant_type") != "refresh_token" {
			t.Fatal("invalid refresh request")
		}
		body, _ := json.Marshal(map[string]any{"access_token": testAccessToken(), "expires_in": 3600})
		return tokenResponse(string(body)), nil
	})}
	tokens, err := Refresh(t.Context(), client, "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.RefreshToken != "old-refresh" {
		t.Fatal("lost an unrotated refresh token")
	}
}

func TestTokenErrorsDoNotExposeSecretsOrFollowRedirects(t *testing.T) {
	for _, status := range []int{200, 400, 307} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				response := tokenResponse(`{"access_token":"private-token","error_description":"private-token"}`)
				response.StatusCode = status
				response.Header.Set("Location", "https://example.com/collect")
				return response, nil
			})}
			_, err := Refresh(t.Context(), client, "private-token")
			if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unsafe token error: %v", err)
			}
			if calls != 1 {
				t.Fatalf("followed a token endpoint redirect: %d requests", calls)
			}
		})
	}
}
