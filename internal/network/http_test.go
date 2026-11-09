package network

import (
	"net/http"
	"strings"
	"testing"
)

func TestExplicitProxyPreservesDestinationBypasses(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://environment.invalid:8080")
	t.Setenv("HTTPS_PROXY", "http://environment.invalid:8080")
	t.Setenv("NO_PROXY", "internal.example,192.0.2.0/24")
	t.Setenv("no_proxy", "")
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			want := scheme + "://user:password@proxy.example:1080"
			proxy, err := Proxy(want)
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{"http://public.example", "https://public.example"} {
				request, _ := http.NewRequest(http.MethodGet, target, nil)
				got, err := proxy(request)
				if err != nil || got == nil || got.String() != want {
					t.Fatalf("proxy for %s = %v, %v", target, got, err)
				}
			}
			for _, target := range []string{"http://localhost:11434", "http://127.0.0.1", "http://[::1]", "https://internal.example", "https://child.internal.example", "http://192.0.2.10"} {
				request, _ := http.NewRequest(http.MethodGet, target, nil)
				if got, err := proxy(request); err != nil || got != nil {
					t.Fatalf("bypass for %s = %v, %v", target, got, err)
				}
			}
		})
	}
}

func TestProxyRejectsInvalidURLsWithoutExposingCredentials(t *testing.T) {
	for _, raw := range []string{
		"proxy.example:8080", "http://", "ftp://proxy.example", "http://proxy.example:0", "http://proxy.example:65536",
		"http://user:private-password@proxy.example/path", "http://user:private-password@proxy.example:bad",
		"http://proxy.example?secret=value", "http://proxy.example#fragment",
	} {
		_, err := Proxy(raw)
		if err == nil {
			t.Errorf("accepted %q", raw)
		} else if strings.Contains(err.Error(), "private-password") || strings.Contains(err.Error(), "secret=value") {
			t.Errorf("proxy error exposed credentials: %v", err)
		}
	}
}

func TestExplicitProxyPreservesNoProxyEnvironmentPrecedence(t *testing.T) {
	for _, test := range []struct{ upper, lower, bypass string }{
		{"upper.example", "lower.example", "upper.example"},
		{"", "lower.example", "lower.example"},
		{"", "", ""},
	} {
		t.Run(test.upper+"/"+test.lower, func(t *testing.T) {
			t.Setenv("NO_PROXY", test.upper)
			t.Setenv("no_proxy", test.lower)
			proxy, err := Proxy("http://proxy.example:8080")
			if err != nil {
				t.Fatal(err)
			}
			for _, host := range []string{"upper.example", "lower.example"} {
				request, _ := http.NewRequest(http.MethodGet, "https://"+host, nil)
				got, err := proxy(request)
				if err != nil || (got == nil) != (host == test.bypass) {
					t.Fatalf("proxy for %s = %v, %v; bypass = %s", host, got, err, test.bypass)
				}
			}
		})
	}
}
