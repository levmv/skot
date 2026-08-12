package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type webPolicyError struct {
	message string
}

const nonPublicWebDestinationMessage = "private, local, and special-purpose destinations are not allowed"

func (err *webPolicyError) Error() string { return err.message }

func newWebPolicyError(message string) error { return &webPolicyError{message: message} }

func isWebPolicyError(err error) bool {
	var target *webPolicyError
	return errors.As(err, &target)
}

type httpFetchBackend struct {
	client *http.Client
}

func newHTTPFetchBackend() *httpFetchBackend {
	return &httpFetchBackend{client: safeWebHTTPClient()}
}

func (*httpFetchBackend) Name() string { return "http" }

func (backend *httpFetchBackend) Fetch(ctx context.Context, request webFetchRequest) (webFetchResult, error) {
	target, err := validatePublicURL(request.URL)
	if err != nil {
		return webFetchResult{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return webFetchResult{}, err
	}
	httpRequest.Header.Set("Accept", "text/html, text/plain, application/json;q=0.8")
	httpRequest.Header.Set("User-Agent", "Skot/1 web-fetch")
	response, err := backend.client.Do(httpRequest)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("request URL: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return webFetchResult{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, webMaxResponseBytes+1))
	if err != nil {
		return webFetchResult{}, fmt.Errorf("read page: %w", err)
	}
	truncated := len(raw) > webMaxResponseBytes
	if truncated {
		raw = raw[:webMaxResponseBytes]
	}
	text, title, err := extractWebContent(raw, strings.ToLower(response.Header.Get("Content-Type")))
	if err != nil {
		return webFetchResult{}, err
	}
	finalURL := target.String()
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL.String()
	}
	return webFetchResult{URL: finalURL, Title: title, Text: text, Truncated: truncated}, nil
}

func safeWebHTTPClient() *http.Client {
	transport := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	// A proxy resolves and connects on our behalf, bypassing safeWebDial's DNS
	// checks. Direct fetching keeps the SSRF boundary local and auditable.
	transport.Proxy = nil
	transport.DialContext = safeWebDial
	return &http.Client{
		Transport: transport,
		Timeout:   45 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			_, err := validatePublicURL(request.URL.String())
			return err
		},
	}
}

func safeWebDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		if !isPublicWebIP(address) {
			return nil, newWebPolicyError(fmt.Sprintf("destination %s resolves to a non-public address", host))
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("destination %s has no address", host)
	}
	dialer := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
}

func validatePublicURL(raw string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Hostname() == "" {
		return nil, newWebPolicyError("URL must be absolute")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, newWebPolicyError("URL scheme must be http or https")
	}
	if target.User != nil {
		return nil, newWebPolicyError("URL credentials are not allowed")
	}
	hostname := strings.ToLower(target.Hostname())
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") {
		return nil, newWebPolicyError(nonPublicWebDestinationMessage)
	}
	if address := net.ParseIP(hostname); address != nil && !isPublicWebIP(address) {
		return nil, newWebPolicyError(nonPublicWebDestinationMessage)
	}
	return target, nil
}

func isPublicWebIP(address net.IP) bool {
	parsed, ok := netip.AddrFromSlice(address)
	if !ok {
		return false
	}
	parsed = parsed.Unmap()
	if !parsed.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicWebPrefixes {
		if prefix.Contains(parsed) {
			return false
		}
	}
	if wellKnownNAT64Prefix.Contains(parsed) {
		bytes := parsed.As16()
		translated := netip.AddrFrom4([4]byte{bytes[12], bytes[13], bytes[14], bytes[15]})
		return isPublicWebAddr(translated)
	}
	return true
}

func isPublicWebAddr(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicWebPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var wellKnownNAT64Prefix = netip.MustParsePrefix("64:ff9b::/96")

// Conservatively reject IANA special-purpose ranges which are wholly or
// predominantly not globally reachable. IsPrivate alone only covers RFC 1918
// and IPv6 ULA, leaving shared, benchmarking, documentation, transition, and
// reserved ranges available for SSRF into a deployment that routes them
// internally. The few anycast exceptions inside broader reserved blocks are
// not useful enough to web_fetch to weaken this boundary.
var nonPublicWebPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("fe80::/10"),
}

func extractWebContent(raw []byte, contentType string) (text, title string, err error) {
	prefix := strings.ToLower(string(raw[:min(len(raw), 256)]))
	switch {
	case strings.Contains(contentType, "text/html") || strings.Contains(prefix, "<html"):
		document, parseErr := html.Parse(strings.NewReader(string(raw)))
		if parseErr != nil {
			return "", "", fmt.Errorf("parse HTML: %w", parseErr)
		}
		if titleNode := findWebElement(document, func(node *html.Node) bool { return node.Data == "title" }); titleNode != nil {
			title = compactWebText(webNodeText(titleNode), 500)
		}
		contentRoot := findWebContentRoot(document)
		var blocks []string
		var walk func(*html.Node)
		walk = func(node *html.Node) {
			if node.Type == html.ElementNode {
				if skipWebElement(node.Data) {
					return
				}
				if webContentBlock(node.Data) {
					if value := compactWebText(webNodeText(node), 8_000); value != "" {
						if len(blocks) == 0 || blocks[len(blocks)-1] != value {
							blocks = append(blocks, value)
						}
					}
					return
				}
			}
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				walk(child)
			}
		}
		walk(contentRoot)
		if len(blocks) == 0 {
			return compactWebText(webNodeText(contentRoot), webMaxTextBytes), title, nil
		}
		return strings.Join(blocks, "\n\n"), title, nil
	case strings.Contains(contentType, "text/") || strings.Contains(contentType, "json") || contentType == "":
		return strings.TrimSpace(string(raw)), "", nil
	default:
		return "", "", fmt.Errorf("unsupported content type %q", contentType)
	}
}

func findWebContentRoot(document *html.Node) *html.Node {
	if main := findWebElement(document, func(node *html.Node) bool {
		return node.Data == "main" || hasWebAttribute(node, "role", "main")
	}); main != nil {
		return main
	}
	if article := findUniqueWebElement(document, func(node *html.Node) bool { return node.Data == "article" }); article != nil {
		return article
	}
	if body := findWebElement(document, func(node *html.Node) bool { return node.Data == "body" }); body != nil {
		return body
	}
	return document
}

func findWebElement(root *html.Node, matches func(*html.Node) bool) *html.Node {
	if root.Type == html.ElementNode && matches(root) {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := findWebElement(child, matches); found != nil {
			return found
		}
	}
	return nil
}

func findUniqueWebElement(root *html.Node, matches func(*html.Node) bool) *html.Node {
	var found *html.Node
	var multiple bool
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if multiple {
			return
		}
		if node.Type == html.ElementNode && matches(node) {
			if found != nil {
				multiple = true
				return
			}
			found = node
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	if multiple {
		return nil
	}
	return found
}

func hasWebAttribute(node *html.Node, key, value string) bool {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, key) && strings.EqualFold(strings.TrimSpace(attribute.Val), value) {
			return true
		}
	}
	return false
}

func skipWebElement(tag string) bool {
	switch tag {
	case "script", "style", "noscript", "svg", "canvas", "nav", "footer", "header", "aside", "form", "dialog", "menu", "template", "iframe":
		return true
	default:
		return false
	}
}

func webContentBlock(tag string) bool {
	switch tag {
	case "p", "li", "pre", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6", "td", "th", "dt", "dd", "figcaption":
		return true
	default:
		return false
	}
}

func webNodeText(node *html.Node) string {
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.ElementNode && skipWebElement(current.Data) {
			return
		}
		if current.Type == html.TextNode {
			out.WriteString(current.Data)
			out.WriteByte(' ')
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return out.String()
}
