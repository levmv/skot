package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const (
	hackerNewsAPIBase          = "https://hacker-news.firebaseio.com/v0"
	hackerNewsRequestTimeout   = 20 * time.Second
	hackerNewsMaxResponseBytes = 1 * 1024 * 1024
	hackerNewsMaxRoots         = 15
	hackerNewsMaxDepth         = 3
	hackerNewsMaxComments      = 45
	hackerNewsMaxCommentRunes  = 1_200
	hackerNewsMaxConcurrency   = 8
)

type hackerNewsFetchBackend struct {
	client *hackerNewsClient
}

func newHackerNewsFetchBackend() *hackerNewsFetchBackend {
	return &hackerNewsFetchBackend{client: &hackerNewsClient{
		apiBase: hackerNewsAPIBase,
		client:  &http.Client{Timeout: hackerNewsRequestTimeout},
	}}
}

func (*hackerNewsFetchBackend) Name() string { return "hacker_news" }

func (*hackerNewsFetchBackend) Match(request webFetchRequest) bool {
	_, err := parseHackerNewsItemID(request.URL)
	return err == nil
}

func (backend *hackerNewsFetchBackend) Fetch(ctx context.Context, request webFetchRequest) (webFetchResult, error) {
	thread, err := backend.client.thread(ctx, request.URL)
	if err != nil {
		return webFetchResult{}, err
	}
	return webFetchResult{
		URL: hackerNewsItemURL(thread.story.id), Title: thread.story.title,
		Text: formatHackerNewsThread(thread),
	}, nil
}

type hackerNewsClient struct {
	apiBase string
	client  *http.Client
}

type hackerNewsAPIItem struct {
	ID          int64   `json:"id"`
	Deleted     bool    `json:"deleted"`
	Type        string  `json:"type"`
	By          string  `json:"by"`
	Time        int64   `json:"time"`
	Text        string  `json:"text"`
	Dead        bool    `json:"dead"`
	Kids        []int64 `json:"kids"`
	URL         string  `json:"url"`
	Score       int     `json:"score"`
	Title       string  `json:"title"`
	Descendants int     `json:"descendants"`
}

type hackerNewsStory struct {
	id          int64
	kind        string
	by          string
	publishedAt time.Time
	title       string
	text        string
	url         string
	score       int
	comments    int
}

type hackerNewsComment struct {
	id       int64
	by       string
	text     string
	children []*hackerNewsComment
}

type hackerNewsThread struct {
	story   hackerNewsStory
	roots   []*hackerNewsComment
	warning string
}

func (client *hackerNewsClient) thread(ctx context.Context, reference string) (hackerNewsThread, error) {
	id, err := parseHackerNewsItemID(reference)
	if err != nil {
		return hackerNewsThread{}, err
	}
	item, err := client.fetchItem(ctx, id)
	if err != nil {
		return hackerNewsThread{}, err
	}
	if item.Deleted || item.Dead {
		return hackerNewsThread{}, fmt.Errorf("hacker news item %d is deleted or dead", id)
	}
	if item.Type != "story" && item.Type != "job" && item.Type != "poll" {
		return hackerNewsThread{}, fmt.Errorf("hacker news item %d is a %s, not a discussion", id, item.Type)
	}
	roots, warning := client.collectComments(ctx, item.Kids)
	if err := ctx.Err(); err != nil {
		return hackerNewsThread{}, err
	}
	return hackerNewsThread{story: hackerNewsStoryFromItem(item), roots: roots, warning: warning}, nil
}

func parseHackerNewsItemID(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
		return id, nil
	}
	target, err := url.Parse(raw)
	if err != nil || target.Hostname() == "" {
		return 0, errors.New("hacker news item must be a positive ID or item URL")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return 0, errors.New("hacker news item URL must use http or https")
	}
	host := strings.TrimPrefix(strings.ToLower(target.Hostname()), "www.")
	if host != "news.ycombinator.com" || strings.Trim(target.Path, "/") != "item" {
		return 0, errors.New("hacker news item URL must use news.ycombinator.com/item")
	}
	id, err := strconv.ParseInt(target.Query().Get("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("hacker news item URL has no valid id")
	}
	return id, nil
}

func hackerNewsItemURL(id int64) string {
	return fmt.Sprintf("https://news.ycombinator.com/item?id=%d", id)
}

func (client *hackerNewsClient) fetchItem(ctx context.Context, id int64) (hackerNewsAPIItem, error) {
	target := fmt.Sprintf("%s/item/%d.json", strings.TrimRight(client.apiBase, "/"), id)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return hackerNewsAPIItem{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Skot/1 hacker-news")
	response, err := client.client.Do(request)
	if err != nil {
		return hackerNewsAPIItem{}, fmt.Errorf("request Hacker News item %d: %w", id, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		drainWebError(response.Body)
		return hackerNewsAPIItem{}, fmt.Errorf("hacker news item %d returned HTTP %d", id, response.StatusCode)
	}
	limited := io.LimitReader(response.Body, hackerNewsMaxResponseBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return hackerNewsAPIItem{}, err
	}
	if len(raw) > hackerNewsMaxResponseBytes {
		return hackerNewsAPIItem{}, errors.New("hacker news response exceeds size limit")
	}
	if string(raw) == "null" {
		return hackerNewsAPIItem{}, fmt.Errorf("hacker news item %d was not found", id)
	}
	var item hackerNewsAPIItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return hackerNewsAPIItem{}, fmt.Errorf("decode Hacker News item %d: %w", id, err)
	}
	return item, nil
}

type hackerNewsItemResult struct {
	item hackerNewsAPIItem
	err  error
}

func (client *hackerNewsClient) fetchItems(ctx context.Context, ids []int64) []hackerNewsItemResult {
	results := make([]hackerNewsItemResult, len(ids))
	semaphore := make(chan struct{}, hackerNewsMaxConcurrency)
	var wait sync.WaitGroup
	for index, id := range ids {
		wait.Add(1)
		go func(index int, id int64) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[index].err = ctx.Err()
				return
			}
			results[index].item, results[index].err = client.fetchItem(ctx, id)
		}(index, id)
	}
	wait.Wait()
	return results
}

type hackerNewsCommentTask struct {
	id     int64
	parent *hackerNewsComment
}

func (client *hackerNewsClient) collectComments(ctx context.Context, ids []int64) ([]*hackerNewsComment, string) {
	rootCount := min(len(ids), hackerNewsMaxRoots)
	current := make([]hackerNewsCommentTask, 0, rootCount)
	for _, id := range ids[:rootCount] {
		current = append(current, hackerNewsCommentTask{id: id})
	}
	scheduled := len(current)
	roots := make([]*hackerNewsComment, 0, rootCount)
	failed := 0
	var firstErr error
	for depth := 0; depth < hackerNewsMaxDepth && len(current) > 0; depth++ {
		batch := make([]int64, len(current))
		for index, task := range current {
			batch[index] = task.id
		}
		results := client.fetchItems(ctx, batch)
		next := make([]hackerNewsCommentTask, 0)
		for index, result := range results {
			if result.err != nil {
				failed++
				if firstErr == nil {
					firstErr = result.err
				}
				continue
			}
			item := result.item
			if item.Deleted || item.Dead || item.Type != "comment" {
				continue
			}
			text := truncateHackerNewsRunes(cleanHackerNewsHTML(item.Text), hackerNewsMaxCommentRunes)
			if text == "" {
				continue
			}
			comment := &hackerNewsComment{id: item.ID, by: cleanHackerNewsLine(item.By), text: text}
			if current[index].parent == nil {
				roots = append(roots, comment)
			} else {
				current[index].parent.children = append(current[index].parent.children, comment)
			}
			if depth+1 >= hackerNewsMaxDepth {
				continue
			}
			for _, childID := range item.Kids {
				if scheduled >= hackerNewsMaxComments {
					break
				}
				next = append(next, hackerNewsCommentTask{id: childID, parent: comment})
				scheduled++
			}
		}
		current = next
	}
	if failed > 0 {
		return roots, fmt.Sprintf("%d comment request(s) failed; first error: %v", failed, firstErr)
	}
	return roots, ""
}

func hackerNewsStoryFromItem(item hackerNewsAPIItem) hackerNewsStory {
	publishedAt := time.Time{}
	if item.Time > 0 {
		publishedAt = time.Unix(item.Time, 0).UTC()
	}
	return hackerNewsStory{
		id: item.ID, kind: cleanHackerNewsLine(item.Type), by: cleanHackerNewsLine(item.By),
		publishedAt: publishedAt, title: cleanHackerNewsLine(cleanHackerNewsHTML(item.Title)),
		text: cleanHackerNewsHTML(item.Text), url: normalizeWebResultURL(item.URL),
		score: item.Score, comments: item.Descendants,
	}
}

func formatHackerNewsThread(thread hackerNewsThread) string {
	story := thread.story
	title := story.title
	if title == "" {
		title = fmt.Sprintf("Hacker News item %d", story.id)
	}
	var out strings.Builder
	out.WriteString("# Hacker News item\n\n")
	writeHackerNewsField(&out, "Title", title)
	writeHackerNewsField(&out, "HN URL", hackerNewsItemURL(story.id))
	writeHackerNewsField(&out, "Article URL", story.url)
	writeHackerNewsField(&out, "Posted by", story.by)
	if !story.publishedAt.IsZero() {
		writeHackerNewsField(&out, "Published at", story.publishedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&out, "Score: %d\nTotal comments: %d\n", story.score, story.comments)
	if story.kind != "" && story.kind != "story" {
		writeHackerNewsField(&out, "Type", story.kind)
	}
	if story.text != "" {
		out.WriteString("\n# Hacker News post text\n\n")
		out.WriteString(story.text)
		out.WriteByte('\n')
	}
	out.WriteString("\n# Hacker News discussion\n\n")
	fmt.Fprintf(&out, "Selection: ranked order, up to %d roots, depth %d, maximum %d comments. Hacker News does not expose comment scores.\n\n", hackerNewsMaxRoots, hackerNewsMaxDepth, hackerNewsMaxComments)
	if thread.warning != "" {
		fmt.Fprintf(&out, "Warning: %s\n\n", cleanHackerNewsLine(thread.warning))
	}
	if len(thread.roots) == 0 {
		out.WriteString("No comments loaded.\n")
	} else {
		for _, comment := range thread.roots {
			renderHackerNewsComment(&out, comment, 0)
		}
	}
	return truncateHackerNewsBytes(strings.TrimSpace(out.String()), webMaxTextBytes)
}

func writeHackerNewsField(out *strings.Builder, name, value string) {
	if value = cleanHackerNewsLine(value); value != "" {
		fmt.Fprintf(out, "%s: %s\n", name, value)
	}
}

func renderHackerNewsComment(out *strings.Builder, comment *hackerNewsComment, depth int) {
	if comment == nil {
		return
	}
	author := comment.by
	if author == "" {
		author = "unknown"
	}
	fmt.Fprintf(out, "%s- %s [item %d]: %s\n", strings.Repeat("  ", depth), author, comment.id, cleanHackerNewsLine(comment.text))
	for _, child := range comment.children {
		renderHackerNewsComment(out, child, depth+1)
	}
	if depth == 0 {
		out.WriteByte('\n')
	}
}

func cleanHackerNewsHTML(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	document, err := html.Parse(strings.NewReader("<html><body>" + value + "</body></html>"))
	if err != nil {
		return cleanHackerNewsLine(value)
	}
	var out strings.Builder
	var walk func(*html.Node, bool)
	walk = func(node *html.Node, skipped bool) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "script", "style", "noscript", "svg", "canvas":
				skipped = true
			case "br", "p", "pre", "li", "blockquote":
				out.WriteByte('\n')
			}
		}
		if node.Type == html.TextNode && !skipped {
			out.WriteString(node.Data)
			out.WriteByte(' ')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, skipped)
		}
	}
	walk(document, false)
	lines := strings.Split(strings.ReplaceAll(sanitizeWebText(out.String()), "\r\n", "\n"), "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			cleaned = append(cleaned, line)
		}
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

func cleanHackerNewsLine(value string) string {
	return strings.Join(strings.Fields(sanitizeWebText(value)), " ")
}

func truncateHackerNewsRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit])) + "\n[…truncated…]"
}

func truncateHackerNewsBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut] + "\n[…truncated…]"
}
