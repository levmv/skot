package skills

import (
	"slices"
	"strings"
	"testing"
)

func TestParsePortableMetadataAndPreserveInstructions(t *testing.T) {
	raw := "\ufeff---\r\n" +
		"name: release-review\r\n" +
		"description: >-\r\n  Проверить выпуск\r\n  перед публикацией.\r\n" +
		"license: MIT\r\n" +
		"compatibility: Requires git\r\n" +
		"metadata:\r\n  author: example\r\n  version: '1.0'\r\n" +
		"disable-model-invocation: true\r\nuser-invocable: false\r\n" +
		"allowed-tools: [Read, Bash]\r\nmodel: example/model\r\n" +
		"---\r\n\r\n# Review\r\n\r\nRead references/checklist.md.\r\n"
	document, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	meta := document.Metadata
	if meta.Name != "release-review" || meta.Description != "Проверить выпуск перед публикацией." ||
		meta.License != "MIT" || meta.Compatibility != "Requires git" ||
		meta.Metadata["author"] != "example" || meta.Metadata["version"] != "1.0" {
		t.Fatalf("metadata = %#v", meta)
	}
	if !meta.DisableModelInvocation || meta.UserInvocable ||
		!slices.Equal(meta.UnhandledFields, []string{"allowed-tools", "model"}) {
		t.Fatalf("invocation metadata = %#v", meta)
	}
	if document.Raw != raw || document.Body != "\r\n# Review\r\n\r\nRead references/checklist.md.\r\n" {
		t.Fatalf("document text was changed: %#v", document)
	}
}

func TestParseRejectsInvalidFrontmatter(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{"missing frontmatter", "# Instructions\n"},
		{"unclosed frontmatter", "---\nname: review\n"},
		{"missing description", "---\nname: review\n---\n"},
		{"numeric description", "---\nname: review\ndescription: 123\n---\n"},
		{"empty description", "---\nname: review\ndescription: '  '\n---\n"},
		{"duplicate field", "---\nname: review\nname: other\ndescription: Review.\n---\n"},
		{"sequence", "---\n- name\n- review\n---\n"},
		{"non-string key", "---\nname: review\ndescription: Review.\n2: bad\n---\n"},
		{"invalid name", "---\nname: Review\ndescription: Review.\n---\n"},
		{"uppercase Unicode name", "---\nname: Обзор\ndescription: Review.\n---\n"},
		{"double hyphen", "---\nname: code--review\ndescription: Review.\n---\n"},
		{"long name", "---\nname: " + strings.Repeat("a", 65) + "\ndescription: Review.\n---\n"},
		{"long Unicode name", "---\nname: " + strings.Repeat("я", 65) + "\ndescription: Review.\n---\n"},
		{"long description", "---\nname: review\ndescription: " + strings.Repeat("я", 1025) + "\n---\n"},
		{"wrong flag type", "---\nname: review\ndescription: Review.\ndisable-model-invocation: 'false'\n---\n"},
		{"wrong metadata type", "---\nname: review\ndescription: Review.\nmetadata:\n  version: 1\n---\n"},
		{"invalid YAML", "---\nname: review\ndescription: [\n---\n"},
		{"trailing YAML document", "---\nname: review\ndescription: Review.\n...\nname: ignored\n---\n"},
		{"binary body", "---\nname: review\ndescription: Review.\n---\n\x00"},
		{"invalid UTF-8", "---\nname: review\ndescription: Review.\n---\n\xff"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Parse(test.raw); err == nil {
				t.Fatal("invalid document was accepted")
			}
		})
	}
}

func TestParseUnicodeNamesAndCharacterLimits(t *testing.T) {
	description := strings.Repeat("я", 1024)
	for _, name := range []string{strings.Repeat("я", 64), "数据-分析", "révision-２"} {
		document, err := Parse("---\nname: " + name + "\ndescription: " + description + "\n---\nInstructions.\n")
		if err != nil {
			t.Fatal(err)
		}
		if document.Metadata.Name != name || document.Metadata.Description != description || document.Metadata.DisableModelInvocation || !document.Metadata.UserInvocable {
			t.Fatalf("metadata = %#v", document.Metadata)
		}
	}
}

func TestParseBoundsFrontmatterAndDocument(t *testing.T) {
	for _, raw := range []string{
		"---\n# " + strings.Repeat("x", MaxFrontmatterBytes) + "\nname: review\ndescription: Review.\n---\n",
		"---\nname: review\ndescription: Review.\n---\n" + strings.Repeat("x", MaxDocumentBytes),
	} {
		if _, err := Parse(raw); err == nil {
			t.Fatal("oversized document was accepted")
		}
	}
}

func TestParseBlockScalarMayContainIndentedDelimiter(t *testing.T) {
	document, err := Parse("---\nname: review\ndescription: |\n  Review changes.\n  ---\n  Then report.\n---\nBody.\n")
	if err != nil {
		t.Fatal(err)
	}
	if document.Metadata.Description != "Review changes.\n---\nThen report." || document.Body != "Body.\n" {
		t.Fatalf("document = %#v", document)
	}
}
