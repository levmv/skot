package skills

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	// MaxFrontmatterBytes is the byte limit for YAML frontmatter, including
	// its opening and closing delimiter lines.
	MaxFrontmatterBytes = 64 * 1024
	// MaxDocumentBytes is the byte limit for a complete SKILL.md document.
	MaxDocumentBytes = 1024 * 1024
)

// Metadata contains the parsed SKILL.md frontmatter.
type Metadata struct {
	Name                   string
	Description            string
	License                string
	Compatibility          string
	Metadata               map[string]string
	DisableModelInvocation bool
	UserInvocable          bool
	// UnhandledFields lists unrecognized field names. Values remain in Document.Raw.
	UnhandledFields []string
}

// Document contains parsed metadata and the original body and file text.
type Document struct {
	Metadata Metadata
	Body     string
	Raw      string
}

// Parse validates a document, preserving Body and Raw including line endings.
// Input is limited by MaxDocumentBytes and MaxFrontmatterBytes.
func Parse(raw string) (Document, error) {
	if len(raw) > MaxDocumentBytes {
		return Document{}, fmt.Errorf("SKILL.md exceeds %d bytes", MaxDocumentBytes)
	}
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return Document{}, errors.New("SKILL.md must be UTF-8 text without NUL bytes")
	}
	metadata, offset, err := readMetadata(strings.NewReader(raw))
	if err != nil {
		return Document{}, err
	}
	return Document{Metadata: metadata, Body: raw[offset:], Raw: raw}, nil
}

// Discovery reads only the frontmatter, leaving instruction bodies for Load.
func readMetadata(input io.Reader) (Metadata, int, error) {
	reader := bufio.NewReader(io.LimitReader(input, MaxFrontmatterBytes+1))
	var header strings.Builder
	offset := 0
	for first := true; ; first = false {
		line, err := reader.ReadString('\n')
		offset += len(line)
		if offset > MaxFrontmatterBytes {
			return Metadata{}, 0, fmt.Errorf("frontmatter exceeds %d bytes", MaxFrontmatterBytes)
		}
		delimiter := strings.TrimRight(line, " \t\r\n")
		if first {
			if strings.TrimPrefix(delimiter, "\ufeff") != "---" {
				return Metadata{}, 0, errors.New("SKILL.md must begin with YAML frontmatter (---)")
			}
		} else if delimiter == "---" {
			metadata, parseErr := parseMetadata(header.String())
			return metadata, offset, parseErr
		} else {
			header.WriteString(line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return Metadata{}, 0, errors.New("frontmatter is missing its closing ---")
			}
			return Metadata{}, 0, fmt.Errorf("read frontmatter: %w", err)
		}
	}
}

func parseMetadata(header string) (Metadata, error) {
	var value any
	decoder := yaml.NewDecoder(strings.NewReader(header))
	if err := decoder.Decode(&value); err != nil {
		return Metadata{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Metadata{}, errors.New("frontmatter must contain exactly one YAML document")
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return Metadata{}, errors.New("frontmatter must be a mapping with string keys")
	}
	metadata := Metadata{UserInvocable: true}
	for _, field := range []struct {
		name     string
		target   *string
		required bool
		limit    int
	}{
		{"name", &metadata.Name, true, 64},
		{"description", &metadata.Description, true, 1024},
		{"license", &metadata.License, false, 0},
		{"compatibility", &metadata.Compatibility, false, 500},
	} {
		value, present := fields[field.name]
		if !present && !field.required {
			continue
		}
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return Metadata{}, fmt.Errorf("%s must be a non-empty string", field.name)
		}
		text = strings.TrimSpace(text)
		if field.limit > 0 && utf8.RuneCountInString(text) > field.limit {
			return Metadata{}, fmt.Errorf("%s exceeds %d characters", field.name, field.limit)
		}
		*field.target = text
		delete(fields, field.name)
	}
	if !validName(metadata.Name) {
		return Metadata{}, errors.New("name must use lowercase letters, numbers, and single hyphens between words")
	}
	for _, field := range []struct {
		name   string
		target *bool
	}{
		{"disable-model-invocation", &metadata.DisableModelInvocation},
		{"user-invocable", &metadata.UserInvocable},
	} {
		if value, present := fields[field.name]; present {
			flag, ok := value.(bool)
			if !ok {
				return Metadata{}, fmt.Errorf("%s must be a boolean", field.name)
			}
			*field.target = flag
			delete(fields, field.name)
		}
	}
	if value, present := fields["metadata"]; present {
		values, ok := value.(map[string]any)
		if !ok {
			return Metadata{}, errors.New("metadata must map strings to strings")
		}
		metadata.Metadata = make(map[string]string, len(values))
		for key, value := range values {
			text, ok := value.(string)
			if !ok {
				return Metadata{}, fmt.Errorf("metadata value %q must be a string", key)
			}
			metadata.Metadata[key] = text
		}
		delete(fields, "metadata")
	}
	for name := range fields {
		metadata.UnhandledFields = append(metadata.UnhandledFields, name)
	}
	slices.Sort(metadata.UnhandledFields)
	return metadata, nil
}

func validName(name string) bool {
	if name == "" || name != strings.ToLower(name) || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return false
	}
	for _, char := range name {
		if char != '-' && !unicode.IsLetter(char) && !unicode.IsNumber(char) {
			return false
		}
	}
	return true
}
