package ui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

const compactToolItemSeparator = "\x00"

type compactToolCall struct {
	Text      string
	GroupKey  string
	GroupDir  string
	GroupItem string
}

func splitToolDisplay(text string) (name, arguments string) {
	if index := strings.IndexAny(text, " \t"); index >= 0 {
		return text[:index], text[index:]
	}
	return text, ""
}

func describeToolCall(name, rawArguments string) compactToolCall {
	name = strings.TrimSpace(name)
	fallback := compactToolCall{Text: compactSingleLine(name, 80)}
	if fallback.Text == "" {
		fallback.Text = "tool"
	}

	switch name {
	case "read":
		var args struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if !decodeToolDisplayArgs(rawArguments, &args) || strings.TrimSpace(args.Path) == "" {
			return fallback
		}
		cleaned := cleanDisplayPath(args.Path)
		dir, item := path.Dir(cleaned), path.Base(cleaned)
		if args.Offset > 1 {
			item += fmt.Sprintf(":%d", args.Offset)
			if args.Limit > 0 {
				item += fmt.Sprintf("+%d", args.Limit)
			}
		}
		return compactToolCall{
			Text:      formatReadGroup(dir, []string{item}),
			GroupKey:  "read\x00" + dir,
			GroupDir:  dir,
			GroupItem: item,
		}
	case "ls":
		var args struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
		}
		if !decodeToolDisplayArgs(rawArguments, &args) {
			return fallback
		}
		text := "list  " + cleanDisplayPath(args.Path)
		if args.Offset > 1 {
			text += fmt.Sprintf(":%d", args.Offset)
		}
		return compactToolCall{Text: text}
	case "grep":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
			Include string `json:"include"`
		}
		if !decodeToolDisplayArgs(rawArguments, &args) {
			return fallback
		}
		parts := []string{"grep  " + quoteToolValue(args.Pattern, 100)}
		if strings.TrimSpace(args.Path) != "" {
			parts = append(parts, cleanDisplayPath(args.Path))
		}
		if strings.TrimSpace(args.Include) != "" {
			parts = append(parts, compactSingleLine(args.Include, 80))
		}
		return compactToolCall{Text: strings.Join(parts, " · ")}
	case "glob":
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if !decodeToolDisplayArgs(rawArguments, &args) {
			return fallback
		}
		text := "glob  " + compactSingleLine(args.Pattern, 120)
		if strings.TrimSpace(args.Path) != "" {
			text += " · " + cleanDisplayPath(args.Path)
		}
		return compactToolCall{Text: text}
	case "edit", "write":
		var args struct {
			Path string `json:"path"`
		}
		if !decodeToolDisplayArgs(rawArguments, &args) || strings.TrimSpace(args.Path) == "" {
			return fallback
		}
		return compactToolCall{Text: name + "  " + cleanDisplayPath(args.Path)}
	case "bash":
		var args struct {
			Command    string `json:"command"`
			Workdir    string `json:"workdir"`
			Background bool   `json:"background"`
		}
		if !decodeToolDisplayArgs(rawArguments, &args) || strings.TrimSpace(args.Command) == "" {
			return fallback
		}
		text := "$ " + compactCommand(args.Command, 180)
		if strings.TrimSpace(args.Workdir) != "" {
			text += " · in " + cleanDisplayPath(args.Workdir)
		}
		if args.Background {
			text += " · background"
		}
		return compactToolCall{Text: text}
	case "job":
		var args struct {
			Action         string `json:"action"`
			JobID          string `json:"job_id"`
			TimeoutSeconds int    `json:"timeout"`
		}
		if !decodeToolDisplayArgs(rawArguments, &args) {
			return fallback
		}
		text := "job  " + compactSingleLine(args.Action, 32)
		if strings.TrimSpace(args.JobID) != "" {
			text += " " + compactSingleLine(args.JobID, 80)
		}
		if args.TimeoutSeconds > 0 {
			text += fmt.Sprintf(" · timeout %ds", args.TimeoutSeconds)
		}
		return compactToolCall{Text: strings.TrimSpace(text)}
	case "agent":
		var args struct {
			Action string   `json:"action"`
			ID     string   `json:"id"`
			IDs    []string `json:"ids"`
			Prompt string   `json:"prompt"`
			Model  string   `json:"model"`
			Wait   string   `json:"wait"`
		}
		if !decodeToolDisplayArgs(rawArguments, &args) || strings.TrimSpace(args.Action) == "" {
			return fallback
		}
		text := "agent  " + compactSingleLine(args.Action, 16)
		if args.ID != "" {
			text += " " + compactSingleLine(args.ID, 40)
		} else if len(args.IDs) != 0 {
			text += " " + compactSingleLine(strings.Join(args.IDs, ", "), 80)
		}
		if args.Wait != "" && args.Wait != "none" {
			text += " · wait " + compactSingleLine(args.Wait, 8)
		}
		if args.Model != "" {
			text += " · " + compactSingleLine(args.Model, 80)
		}
		if args.Prompt != "" {
			text += " · " + quoteToolValue(args.Prompt, 100)
		}
		return compactToolCall{Text: text}
	case "web_search":
		var args struct {
			Query string `json:"query"`
		}
		if !decodeToolDisplayArgs(rawArguments, &args) {
			return fallback
		}
		return compactToolCall{Text: "web  " + quoteToolValue(args.Query, 140)}
	case "web_fetch":
		var args struct {
			URL string `json:"url"`
		}
		if !decodeToolDisplayArgs(rawArguments, &args) {
			return fallback
		}
		return compactToolCall{Text: "fetch  " + compactURL(args.URL)}
	default:
		return fallback
	}
}

func decodeToolDisplayArgs(raw string, target any) bool {
	return strings.TrimSpace(raw) != "" && json.Unmarshal([]byte(raw), target) == nil
}

func cleanDisplayPath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return "."
	}
	return path.Clean(value)
}

func formatReadGroup(dir string, items []string) string {
	if len(items) == 0 {
		return "read"
	}
	if len(items) == 1 {
		if dir == "." || dir == "" {
			return "read  " + items[0]
		}
		return "read  " + path.Join(dir, items[0])
	}
	if dir == "." || dir == "" {
		return "read  " + strings.Join(items, ", ")
	}
	return "read  " + strings.TrimSuffix(dir, "/") + "/ → " + strings.Join(items, ", ")
}

func splitCompactToolItems(items string) []string {
	if items == "" {
		return nil
	}
	return strings.Split(items, compactToolItemSeparator)
}

func quoteToolValue(value string, limit int) string {
	return strconv.Quote(compactSingleLine(value, limit))
}

func compactURL(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return compactSingleLine(raw, 160)
	}
	parsed.User = nil
	hadQuery := parsed.RawQuery != ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	text := parsed.String()
	if hadQuery {
		text += "?…"
	}
	return compactSingleLine(text, 160)
}

func compactSingleLine(value string, limit int) string {
	value = strings.Join(strings.Fields(strings.ToValidUTF8(value, "�")), " ")
	return truncateToolDisplay(value, limit)
}

func compactCommand(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\n", " ↵ ")
	value = strings.ReplaceAll(value, "\t", "⇥")
	return truncateToolDisplay(value, limit)
}

func truncateToolDisplay(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:max(1, limit-1)]) + "…"
}
