package web

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

	"github.com/levmv/skot/internal/webtext"
	"golang.org/x/net/html"
)

// ErrURLNotAllowed identifies a URL rejected by the public-web destination policy.
var ErrURLNotAllowed = errors.New("web destination not allowed")

const nonPublicDestinationMessage = "private, local, and special-purpose destinations are not allowed"

func urlPolicyError(message string) error { return fmt.Errorf("%w: %s", ErrURLNotAllowed, message) }

var directHTTPClient = newPublicHTTPClient()

func (client *Client) fetchHTTP(ctx context.Context, request FetchRequest) (FetchResponse, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, request.URL, nil)
	if err != nil {
		return FetchResponse{}, err
	}
	httpRequest.Header.Set("Accept", "text/html, text/plain, application/json;q=0.8")
	httpRequest.Header.Set("User-Agent", "Skot/1 web-fetch")
	response, err := client.http.Do(httpRequest)
	if err != nil {
		return FetchResponse{}, fmt.Errorf("request URL: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return FetchResponse{}, &HTTPError{StatusCode: response.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return FetchResponse{}, fmt.Errorf("read page: %w", err)
	}
	truncated := len(raw) > maxResponseBytes
	if truncated {
		raw = raw[:maxResponseBytes]
	}
	text, title, err := extractContent(raw, strings.ToLower(response.Header.Get("Content-Type")))
	if err != nil {
		return FetchResponse{}, err
	}
	finalURL := request.URL
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL.String()
	}
	return FetchResponse{URL: finalURL, Title: title, Text: text, Truncated: truncated}, nil
}

func newPublicHTTPClient() *http.Client {
	transport := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	// A proxy resolves and connects on our behalf, bypassing dialPublic's DNS
	// checks. Direct fetching keeps the SSRF boundary local and auditable.
	transport.Proxy = nil
	transport.DialContext = dialPublic
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			_, err := validatePublicURL(request.URL.String())
			return err
		},
	}
}

func dialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		if !isPublicIP(address) {
			return nil, urlPolicyError(fmt.Sprintf("destination %s resolves to a non-public address", host))
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
		return nil, urlPolicyError("URL must be absolute")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, urlPolicyError("URL scheme must be http or https")
	}
	if target.User != nil {
		return nil, urlPolicyError("URL credentials are not allowed")
	}
	hostname := strings.ToLower(target.Hostname())
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") {
		return nil, urlPolicyError(nonPublicDestinationMessage)
	}
	if address := net.ParseIP(hostname); address != nil && !isPublicIP(address) {
		return nil, urlPolicyError(nonPublicDestinationMessage)
	}
	return target, nil
}

func isPublicIP(address net.IP) bool {
	parsed, ok := netip.AddrFromSlice(address)
	if !ok {
		return false
	}
	parsed = parsed.Unmap()
	if !parsed.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(parsed) {
			return false
		}
	}
	if wellKnownNAT64Prefix.Contains(parsed) {
		bytes := parsed.As16()
		translated := netip.AddrFrom4([4]byte{bytes[12], bytes[13], bytes[14], bytes[15]})
		return isPublicAddr(translated)
	}
	return true
}

func isPublicAddr(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
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
var nonPublicPrefixes = []netip.Prefix{
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

func extractContent(raw []byte, contentType string) (text, title string, err error) {
	prefix := strings.ToLower(string(raw[:min(len(raw), 256)]))
	switch {
	case strings.Contains(contentType, "text/html") || strings.Contains(prefix, "<html"):
		document, parseErr := html.Parse(strings.NewReader(string(raw)))
		if parseErr != nil {
			return "", "", fmt.Errorf("parse HTML: %w", parseErr)
		}
		if titleNode := findElement(document, func(node *html.Node) bool { return node.Data == "title" }); titleNode != nil {
			title = webtext.Compact(nodeText(titleNode), maxResponseBytes)
		}
		contentRoot := findContentRoot(document)
		var blocks []string
		var walk func(*html.Node)
		walk = func(node *html.Node) {
			if node.Type == html.ElementNode {
				if skipElement(node.Data) {
					return
				}
				if isContentBlock(node.Data) {
					if value := webtext.Compact(nodeText(node), maxResponseBytes); value != "" {
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
			return webtext.Compact(nodeText(contentRoot), maxResponseBytes), title, nil
		}
		return strings.Join(blocks, "\n\n"), title, nil
	case strings.Contains(contentType, "text/") || strings.Contains(contentType, "json") || contentType == "":
		return strings.TrimSpace(string(raw)), "", nil
	default:
		return "", "", fmt.Errorf("unsupported content type %q", contentType)
	}
}

func findContentRoot(document *html.Node) *html.Node {
	if main := findElement(document, func(node *html.Node) bool {
		return node.Data == "main" || hasAttribute(node, "role", "main")
	}); main != nil {
		return main
	}
	if article := findUniqueElement(document, func(node *html.Node) bool { return node.Data == "article" }); article != nil {
		return article
	}
	if body := findElement(document, func(node *html.Node) bool { return node.Data == "body" }); body != nil {
		return body
	}
	return document
}

func findElement(root *html.Node, matches func(*html.Node) bool) *html.Node {
	if root.Type == html.ElementNode && matches(root) {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := findElement(child, matches); found != nil {
			return found
		}
	}
	return nil
}

func findUniqueElement(root *html.Node, matches func(*html.Node) bool) *html.Node {
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

func hasAttribute(node *html.Node, key, value string) bool {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, key) && strings.EqualFold(strings.TrimSpace(attribute.Val), value) {
			return true
		}
	}
	return false
}

func skipElement(tag string) bool {
	switch tag {
	case "script", "style", "noscript", "svg", "canvas", "nav", "footer", "header", "aside", "form", "dialog", "menu", "template", "iframe":
		return true
	default:
		return false
	}
}

func isContentBlock(tag string) bool {
	switch tag {
	case "p", "li", "pre", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6", "td", "th", "dt", "dd", "figcaption":
		return true
	default:
		return false
	}
}

func nodeText(node *html.Node) string {
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.ElementNode && skipElement(current.Data) {
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
