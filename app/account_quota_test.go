package app

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/levmv/skot/internal/codexauth"
)

func accountQuotaResponse(used int, reset time.Time) *http.Response {
	return codexResponse(http.StatusOK, fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_at":%d}}}`, used, reset.Unix()))
}

func TestCodexQuotaFindsTheWeeklyAccountWindow(t *testing.T) {
	observed := time.Unix(1_800_000_000, 0)
	weekly := `{"used_percent":27,"limit_window_seconds":604800,"reset_at":1800003600}`
	short := `{"used_percent":80,"limit_window_seconds":18000,"reset_at":1800003600}`
	for _, test := range []struct {
		name      string
		body      string
		left      float64
		available bool
	}{
		{"weekly primary", `{"rate_limit":{"primary_window":` + weekly + `,"secondary_window":null}}`, 73, true},
		{"weekly secondary", `{"rate_limit":{"primary_window":` + short + `,"secondary_window":` + weekly + `}}`, 73, true},
		{"unused", `{"rate_limit":{"primary_window":` + strings.Replace(weekly, `:27`, `:0`, 1) + `}}`, 100, true},
		{"exhausted", `{"rate_limit":{"primary_window":` + strings.Replace(weekly, `:27`, `:100`, 1) + `}}`, 0, true},
		{"missing percentage", `{"rate_limit":{"primary_window":{"limit_window_seconds":604800,"reset_at":1800003600}}}`, 0, false},
		{"null percentage", `{"rate_limit":{"primary_window":` + strings.Replace(weekly, `:27`, `:null`, 1) + `}}`, 0, false},
		{"missing reset", `{"rate_limit":{"primary_window":{"used_percent":27,"limit_window_seconds":604800}}}`, 0, false},
		{"reset passed", `{"rate_limit":{"primary_window":` + strings.Replace(weekly, `1800003600`, `1799999999`, 1) + `}}`, 0, false},
		{"invalid percentage", `{"rate_limit":{"primary_window":` + strings.Replace(weekly, `:27`, `:-1`, 1) + `}}`, 0, false},
		{"no account quota", `{"rate_limit":null}`, 0, false},
		{"feature quota only", `{"rate_limit":{"primary_window":` + short + `},"additional_rate_limits":[{"rate_limit":{"primary_window":` + weekly + `}}]}`, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			quota, err := parseCodexAccountQuota([]byte(test.body), observed)
			if err != nil {
				t.Fatal(err)
			}
			if !test.available {
				if quota != (AccountQuota{}) {
					t.Fatalf("unavailable weekly quota = %+v", quota)
				}
				return
			}
			if quota.RemainingPercent != test.left || quota.Window != 7*24*time.Hour ||
				!quota.ObservedAt.Equal(observed) || !quota.ResetsAt.Equal(time.Unix(1_800_003_600, 0)) {
				t.Fatalf("weekly quota = %+v", quota)
			}
		})
	}
}

func TestAccountQuotaRefreshIsThrottledAndExpiresAfterFailuresOrReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		application := newCodexTestApp(t)
		tokens := testCodexTokens()
		calls, fail := 0, false
		reset := time.Now().Add(24 * time.Hour)
		client := &http.Client{Transport: appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if request.Method != http.MethodGet || request.URL.String() != codexUsageURL ||
				request.Header.Get("Authorization") != "Bearer "+tokens.AccessToken ||
				request.Header.Get("ChatGPT-Account-Id") != tokens.AccountID ||
				request.Header.Get("originator") != "skot" || request.Header.Get("x-openai-internal-codex-residency") != "eu" {
				t.Fatal("quota request used the wrong destination or credentials")
			}
			if fail {
				return codexResponse(503, "private-account-details"), nil
			}
			return accountQuotaResponse(27, reset), nil
		})}
		if err := application.refreshAccountQuota(t.Context(), client); err != nil {
			t.Fatal(err)
		}
		initial := application.AccountQuota()
		if initial.Window == 0 || initial.RemainingPercent != 73 {
			t.Fatalf("initial quota = %+v", initial)
		}
		time.Sleep(accountQuotaRefreshInterval - time.Second)
		if err := application.refreshAccountQuota(t.Context(), client); err != nil || calls != 1 {
			t.Fatalf("refresh was not throttled: calls=%d err=%v", calls, err)
		}
		time.Sleep(time.Second)
		fail = true
		if err := application.refreshAccountQuota(t.Context(), client); err == nil || strings.Contains(err.Error(), "private-account-details") || calls != 2 {
			t.Fatalf("failed refresh: calls=%d err=%v", calls, err)
		}
		if got := application.AccountQuota(); got != initial {
			t.Fatalf("recent observation lost after failure: %+v", got)
		}
		time.Sleep(accountQuotaMaxAge - accountQuotaRefreshInterval)
		if got := application.AccountQuota(); got != (AccountQuota{}) {
			t.Fatalf("stale observation still visible: %+v", got)
		}
		fail = false
		reset = time.Now().Add(30 * time.Second)
		if err := application.refreshAccountQuota(t.Context(), client); err != nil || application.AccountQuota().Window == 0 {
			t.Fatalf("did not recover: %v", err)
		}
		time.Sleep(30 * time.Second)
		if got := application.AccountQuota(); got != (AccountQuota{}) {
			t.Fatalf("pre-reset observation still visible: %+v", got)
		}
	})
}

func TestAccountQuotaDiscardsAResponseFromThePreviousLogin(t *testing.T) {
	application := newCodexTestApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	client := &http.Client{Transport: appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return accountQuotaResponse(27, time.Now().Add(time.Hour)), nil
	})}
	done := make(chan error, 1)
	go func() { done <- application.refreshAccountQuota(t.Context(), client) }()
	<-started
	if err := application.Logout(t.Context(), codexauth.Provider); err != nil {
		t.Fatal(err)
	}
	other := testCodexTokens()
	other.AccessToken, other.AccountID = "second-private-access", "account-2"
	login := &codexBrowserLogin{application: application, ctx: t.Context(), tokens: other}
	if err := login.Complete(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := application.AccountQuota(); got != (AccountQuota{}) {
		t.Fatalf("previous account's response became visible: %+v", got)
	}
	client.Transport = appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("ChatGPT-Account-Id") != other.AccountID {
			t.Fatal("refresh used the previous account")
		}
		return accountQuotaResponse(10, time.Now().Add(time.Hour)), nil
	})
	if err := application.refreshAccountQuota(t.Context(), client); err != nil || application.AccountQuota().RemainingPercent != 90 {
		t.Fatalf("new login was not refreshed promptly: %v", err)
	}
	if err := application.Logout(t.Context(), codexauth.Provider); err != nil {
		t.Fatal(err)
	}
	if got := application.AccountQuota(); got != (AccountQuota{}) {
		t.Fatalf("quota still visible after logout: %+v", got)
	}
}

func TestAccountQuotaFollowsTheProviderAndExternallyChangedLogin(t *testing.T) {
	application := newCodexTestApp(t)
	calls := 0
	client := &http.Client{Transport: appRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return accountQuotaResponse(calls*10, time.Now().Add(time.Hour)), nil
	})}
	if err := application.refreshAccountQuota(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	if err := application.config.settings.SetAPIKey(t.Context(), "deepseek", "test-key"); err != nil {
		t.Fatal(err)
	}
	if err := application.SwitchModel(t.Context(), "deepseek/deepseek-v4-flash", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := application.refreshAccountQuota(t.Context(), client); err != nil || calls != 1 || application.AccountQuota() != (AccountQuota{}) {
		t.Fatalf("another provider requested or displayed Codex quota: calls=%d err=%v", calls, err)
	}
	if err := application.SwitchModel(t.Context(), "openai-codex/gpt-6-astra", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := application.AccountQuota(); got.RemainingPercent != 90 || got.Window == 0 {
		t.Fatalf("same account's fresh quota was lost: %+v", got)
	}
	other := testCodexTokens()
	other.AccessToken, other.AccountID = "externally-replaced-access", "account-2"
	saveTestCodexTokens(t, application.config.settings, other)
	if err := application.refreshAccountQuota(t.Context(), client); err != nil || calls != 2 || application.AccountQuota().RemainingPercent != 80 {
		t.Fatalf("external login replacement was not detected: calls=%d err=%v", calls, err)
	}
}

func TestCodexQuotaDoesNotFollowRedirects(t *testing.T) {
	application := newCodexTestApp(t)
	calls := 0
	client := &http.Client{Transport: appRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		response := codexResponse(http.StatusFound, "")
		response.Header.Set("Location", "https://other.example/usage")
		return response, nil
	})}
	if err := application.refreshAccountQuota(t.Context(), client); err == nil || calls != 1 || application.AccountQuota() != (AccountQuota{}) {
		t.Fatalf("redirect handling: calls=%d err=%v", calls, err)
	}
}
