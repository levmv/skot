package web

import (
	"net"
	"strings"
	"testing"
)

func TestPublicWebIPClassification(t *testing.T) {
	for _, address := range []string{"8.8.8.8", "2606:4700:4700::1111", "64:ff9b::808:808"} {
		if !isPublicIP(net.ParseIP(address)) {
			t.Errorf("public address rejected: %s", address)
		}
	}
	for _, address := range []string{
		"100.64.0.1", "192.0.2.1", "198.18.0.1", "203.0.113.1", "240.0.0.1",
		"100::1", "2001:db8::1", "3fff::1", "5f00::1", "fec0::1", "64:ff9b::7f00:1",
	} {
		if isPublicIP(net.ParseIP(address)) {
			t.Errorf("non-public address accepted: %s", address)
		}
	}
}

func TestExtractWebContentPrefersMainAndDropsExecutableMarkup(t *testing.T) {
	raw := []byte(`<html><head><title> Example </title></head><body><nav>ignore me</nav><main><h1>Heading</h1><p>Hello <b>world</b>.</p><script>secret()</script></main><footer>ignore footer</footer></body></html>`)
	text, title, err := extractContent(raw, "text/html")
	if err != nil {
		t.Fatal(err)
	}
	if title != "Example" || !strings.Contains(text, "Heading") || !strings.Contains(text, "Hello world .") || strings.Contains(text, "ignore") || strings.Contains(text, "secret") {
		t.Fatalf("title=%q text=%q", title, text)
	}
}
