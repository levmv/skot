package app

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"html"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/canonicalpath"
	"github.com/levmv/skot/skills"
)

// ClearSkills removes attached instructions and resource access from the
// conversation and pending messages.
func (application *Application) ClearSkills(ctx context.Context) error {
	runtime, err := application.requireRuntime()
	if err != nil {
		return err
	}
	return runtime.ClearInputContext(ctx)
}

// Skills discovers installed packages, excluding protected paths.
func (application *Application) Skills() skills.Catalog {
	return application.discoverSkills(nil)
}

func (application *Application) discoverSkills(paths []string) skills.Catalog {
	sources := application.config.skillSources
	defaults := sources == nil
	project := filepath.Join(application.config.root, ".agents", "skills")
	if defaults {
		sources = []skills.Source{
			{Path: project, Label: "project"},
			{Path: filepath.Join(application.config.home, "skills"), Label: "user"},
		}
	}
	return skills.DiscoverWithOptions(sources, skills.DiscoveryOptions{Paths: paths, AllowPath: func(source skills.Source, path string) bool {
		if defaults && source.Path == project && !canonicalpath.Contains(application.config.root, path) {
			return false
		}
		return application.skillPathAllowed(path)
	}})
}

func (application *Application) skillPathAllowed(path string) bool {
	application.mu.RLock()
	protection := application.state.protection
	application.mu.RUnlock()
	return !protection.Protects(path)
}

// Journal paths are requests, not authority. Only currently installed packages
// may extend workspace reads; exact matches cannot grant a parent directory.
func (application *Application) authorizeSkillPaths(requested []string) []string {
	manifests := make([]string, len(requested))
	for index, path := range requested {
		manifests[index] = filepath.Join(path, "SKILL.md")
	}
	allowed := make(map[string]bool)
	for _, entry := range application.discoverSkills(manifests).Entries {
		allowed[filepath.Dir(entry.Path)] = true
	}
	var paths []string
	for _, path := range requested {
		if allowed[path] {
			paths = append(paths, path)
		}
	}
	return paths
}

// PrepareInput captures skills mentioned in a user message or selected through
// the API. A prepared input can be run or queued without rereading its files.
func (application *Application) PrepareInput(text string, references ...string) (agent.Input, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return agent.Input{}, errors.New("input is empty")
	}
	mentioned := skillReferences(text)
	input := agent.Input{Text: text}
	if len(mentioned) == 0 && len(references) == 0 {
		return input, nil
	}
	catalog := application.Skills()
	selected := make([]string, 0, len(mentioned)+len(references))
	for _, mention := range mentioned {
		reference := mention.reference
		if reference == "" {
			continue
		}
		if _, err := catalog.Resolve(reference); errors.Is(err, skills.ErrNotFound) && !filepath.IsAbs(reference) {
			// Unknown names are ordinary text; broken installed skills are errors.
			for _, diagnostic := range catalog.Diagnostics {
				if filepath.Base(filepath.Dir(diagnostic.Path)) == reference {
					return agent.Input{}, fmt.Errorf("select skill %q: %w", reference, diagnostic.Err)
				}
			}
			continue
		}
		selected = append(selected, reference)
	}
	references = append(selected, references...)
	totalBytes := 0
	seen := make(map[string]bool)
	for _, reference := range references {
		entry, err := catalog.Resolve(reference)
		if err != nil {
			return agent.Input{}, fmt.Errorf("select skill: %w (use $ or /skills to inspect the catalog)", err)
		}
		if seen[entry.Path] {
			continue
		}
		seen[entry.Path] = true
		document, err := entry.Load()
		if err != nil {
			return agent.Input{}, fmt.Errorf("load skill %q: %w", entry.Metadata.Name, err)
		}
		if !document.Metadata.UserInvocable {
			return agent.Input{}, fmt.Errorf("skill %q disables user invocation", document.Metadata.Name)
		}
		var context strings.Builder
		fmt.Fprintf(&context, "<skill name=\"%s\" path=\"%s\" directory=\"%s\">\n",
			html.EscapeString(document.Metadata.Name), html.EscapeString(entry.Path), html.EscapeString(filepath.Dir(entry.Path)))
		context.WriteString(document.Body)
		context.WriteString("\n</skill>")
		totalBytes += context.Len()
		if totalBytes > skills.MaxDocumentBytes {
			return agent.Input{}, errors.New("selected skills exceed the 1 MiB combined input limit")
		}
		input.Instructions = append(input.Instructions, agent.Instruction{Key: entry.Path, Name: document.Metadata.Name, Text: context.String()})
		input.ReadOnlyPaths = append(input.ReadOnlyPaths, filepath.Dir(entry.Path))
	}
	return input, nil
}

// skillReferences skips escaped references and code, but keeps bare $ for completion.
func skillReferences(text string) (references []skillMention) {
	if strings.HasPrefix(strings.TrimSpace(text), "!") {
		return nil
	}
	var codeDelimiter byte
	codeWidth := 0
	for offset := 0; offset < len(text); {
		char := text[offset]
		if char == '\\' && codeWidth == 0 {
			_, width := utf8.DecodeRuneInString(text[offset+1:])
			offset += 1 + width
			continue
		}
		if char == '`' || char == '~' {
			end := offset + 1
			for end < len(text) && text[end] == char {
				end++
			}
			width := end - offset
			lineStart := strings.LastIndexByte(text[:offset], '\n') + 1
			fence := width >= 3 && strings.TrimSpace(text[lineStart:offset]) == ""
			if codeWidth == 0 && (char == '`' || fence) {
				codeDelimiter, codeWidth = char, width
			} else if char == codeDelimiter && (width == codeWidth || codeWidth >= 3 && width >= codeWidth) {
				codeWidth = 0
			}
			offset = end
			continue
		}
		if codeWidth != 0 || char != '$' {
			offset++
			continue
		}
		if previous, _ := utf8.DecodeLastRuneInString(text[:offset]); offset > 0 && !skillReferenceBoundary(previous) {
			offset++
			continue
		}
		start := offset
		token := text[start+1:]
		var reference string
		var consumed int
		quoted := strings.HasPrefix(token, "\"")
		if quoted {
			quoted, quoteErr := strconv.QuotedPrefix(token)
			if quoteErr != nil || json.Unmarshal([]byte(quoted), &reference) != nil {
				offset++
				continue
			}
			consumed = len(quoted)
		} else {
			consumed = skillNamePrefixLength(token)
			reference = token[:consumed]
		}
		end := start + 1 + consumed
		next, _ := utf8.DecodeRuneInString(text[end:])
		// Distinguish sentence punctuation ($name.) from filenames ($name.md).
		dottedWord := false
		if next == '.' {
			afterDot, _ := utf8.DecodeRuneInString(text[end+1:])
			dottedWord = unicode.IsLetter(afterDot) || unicode.IsNumber(afterDot) || afterDot == '_'
		}
		if end < len(text) && (!skillReferenceBoundary(next) || dottedWord) {
			offset = end + 1
			continue
		}
		references = append(references, skillMention{reference: reference, start: start, end: end, quoted: quoted})
		offset = end
	}
	return references
}

func skillReferenceBoundary(char rune) bool {
	return unicode.IsSpace(char) || strings.ContainsRune("()[]{}<>\"'`.,:;!?…—–", char)
}

type skillMention struct {
	reference  string
	start, end int
	quoted     bool
}

// SkillReferenceSpans returns byte ranges [start, end) matching captured selections.
func SkillReferenceSpans(text string, selected []agent.InstructionReference) [][2]int {
	if len(selected) == 0 {
		return nil
	}
	known := make(map[string]bool, len(selected)*2)
	for _, reference := range selected {
		if reference.Key != "" {
			known[reference.Key] = true
		}
		if reference.Name != "" {
			known[reference.Name] = true
		}
	}
	var spans [][2]int
	for _, mention := range skillReferences(text) {
		if !known[mention.reference] {
			continue
		}
		spans = append(spans, [2]int{mention.start, mention.end})
	}
	return spans
}

func skillNamePrefixLength(text string) int {
	for offset, char := range text {
		if unicode.ToLower(char) != char || char != '-' && !unicode.IsLetter(char) && !unicode.IsNumber(char) {
			return offset
		}
	}
	return len(text)
}

// SkillReference returns an invocation for a catalog entry. Paths are quoted
// when a short name would select a different package.
func SkillReference(catalog skills.Catalog, entry skills.Entry) string {
	if resolved, err := catalog.Resolve(entry.Metadata.Name); err == nil && resolved.Path == entry.Path {
		return "$" + entry.Metadata.Name
	}
	quoted, _ := json.Marshal(entry.Path)
	return "$" + string(quoted)
}

// SkillCompletion identifies the skill token at a byte cursor offset.
// Frontends may replace [start:end] with SkillReference.
func SkillCompletion(text string, cursor int) (start, end int, query string, ok bool) {
	if cursor < 0 || cursor > len(text) || !utf8.ValidString(text[:cursor]) {
		return 0, 0, "", false
	}
	for _, mention := range skillReferences(text) {
		if !mention.quoted && cursor > mention.start && cursor <= mention.end {
			return mention.start, mention.end, text[mention.start+1 : cursor], true
		}
	}
	return 0, 0, "", false
}
