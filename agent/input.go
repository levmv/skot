package agent

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/levmv/skot/model"
)

// Input contains a user message and captured instructions retained through compaction.
type Input struct {
	Text         string        `json:"text"`
	Instructions []Instruction `json:"instructions,omitempty"`
	// ReadOnlyPaths requests resource directories after delivery. The host must
	// authorize them through Config.AuthorizeReadOnlyPaths on each tool call.
	ReadOnlyPaths []string `json:"read_only_paths,omitempty"`
}

// Instruction is supplementary user context. A nonempty Key identifies its
// source: resubmission adds text only when it differs from that source's latest
// delivered text. Compaction retains only the latest version of each source.
// An empty Key always adds the instruction and keeps it through compaction.
// Callers supply Text; the runtime journals repeated selections without Text.
type Instruction struct {
	Key             string `json:"key,omitempty"`
	Name            string `json:"name,omitempty"`
	Text            string `json:"text,omitempty"`
	EstimatedTokens int    `json:"estimated_tokens,omitempty"`
}

// InstructionReference describes a selected source without copying its body.
type InstructionReference struct {
	Key             string `json:"key,omitempty"`
	Name            string `json:"name,omitempty"`
	EstimatedTokens int    `json:"estimated_tokens,omitempty"`
}

func (input Input) InstructionReferences() []InstructionReference {
	references := make([]InstructionReference, 0, len(input.Instructions))
	for _, instruction := range input.Instructions {
		tokens := instruction.EstimatedTokens
		if tokens == 0 {
			tokens = estimateTextTokens(instruction.Text)
		}
		references = append(references, InstructionReference{Key: instruction.Key, Name: instruction.Name, EstimatedTokens: tokens})
	}
	return references
}

func (input Input) instructionsText() string {
	if len(input.Instructions) == 0 {
		return ""
	}
	parts := make([]string, len(input.Instructions))
	for index, instruction := range input.Instructions {
		parts[index] = instruction.Text
		if instruction.Text == "" && instruction.Key != "" {
			label := fmt.Sprintf("%q", instruction.Key)
			if instruction.Name != "" {
				label = fmt.Sprintf("%q (source: %q)", instruction.Name, instruction.Key)
			}
			parts[index] = "The user selected " + label + " again. Its instructions are already in this conversation."
		}
	}
	return "Apply these attached instructions when relevant. Resolve relative file paths from the attachment's directory, when given.\n\n" + strings.Join(parts, "\n\n")
}

func (input Input) modelText() string {
	if len(input.Instructions) == 0 {
		return input.Text
	}
	return "User request:\n" + input.Text + "\n\n" + input.instructionsText()
}

const inputTextDetailKind = "input_text"
const inputReferencesDetailKind = "input_references"

func (input Input) item() model.Item {
	return model.Item{Kind: model.ItemUserText, Text: input.modelText(), Details: input.details()}
}

func (input Input) details() []model.Detail {
	if len(input.Instructions) == 0 {
		return nil
	}
	text, _ := NewDetail(inputTextDetailKind, input.Text)
	references, _ := NewDetail(inputReferencesDetailKind, input.InstructionReferences())
	return []model.Detail{text, references}
}

// InputReferences returns the sources selected with this historical message.
func InputReferences(item model.Item) []InstructionReference {
	if item.Kind == model.ItemUserText {
		for _, detail := range item.Details {
			if detail.Kind == inputReferencesDetailKind {
				var references []InstructionReference
				if json.Unmarshal(detail.Data, &references) == nil {
					return references
				}
			}
		}
	}
	return nil
}

// InputText returns the authored message for transcript display and recall.
// The complete model input remains in item.Text.
func InputText(item model.Item) string {
	if item.Kind == model.ItemUserText {
		for _, detail := range item.Details {
			if detail.Kind == inputTextDetailKind {
				var text string
				if json.Unmarshal(detail.Data, &text) == nil {
					return text
				}
			}
		}
	}
	return item.Text
}

func (runtime *Runtime) normalizeUserInput(input Input) (Input, error) {
	text, err := normalizeInput(input.Text)
	if err != nil {
		return Input{}, err
	}
	input.Text = runtime.sanitize(text)
	input.ReadOnlyPaths = slices.Clone(input.ReadOnlyPaths)
	for index, path := range input.ReadOnlyPaths {
		path = runtime.sanitize(path)
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
			return Input{}, fmt.Errorf("resource directory must be an absolute non-root path: %q", path)
		}
		input.ReadOnlyPaths[index] = path
	}
	slices.Sort(input.ReadOnlyPaths)
	input.ReadOnlyPaths = slices.Compact(input.ReadOnlyPaths)
	input.Instructions = slices.Clone(input.Instructions)
	for index := range input.Instructions {
		instruction := &input.Instructions[index]
		instruction.Key = runtime.sanitize(instruction.Key)
		instruction.Name = runtime.sanitize(instruction.Name)
		instruction.Text = runtime.sanitize(instruction.Text)
		instruction.EstimatedTokens = estimateTextTokens(instruction.Text)
	}
	input.Instructions = slices.DeleteFunc(input.Instructions, func(instruction Instruction) bool {
		return strings.TrimSpace(instruction.Text) == ""
	})
	return input, nil
}

func (state State) readOnlyPaths() []string {
	var paths []string
	for _, block := range state.Blocks {
		if block.StartSequence <= state.InputContextClearedThrough {
			continue
		}
		paths = append(paths, block.InputReadOnlyPaths...)
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

func (state State) latestInstructions() map[string]string {
	latest := make(map[string]string)
	for _, block := range state.Blocks {
		if block.StartSequence <= state.InputContextClearedThrough {
			continue
		}
		for _, instruction := range block.InputInstructions {
			if instruction.Key != "" && instruction.Text != "" {
				latest[instruction.Key] = instruction.Text
			}
		}
	}
	return latest
}

// Repeated selections keep a reference without the body. Updating latest also
// deduplicates consecutive queued inputs and detects reverted edits.
func (input Input) withNewInstructions(latest map[string]string) Input {
	input.Instructions = slices.Clone(input.Instructions)
	for index := range input.Instructions {
		instruction := &input.Instructions[index]
		if instruction.Key != "" {
			if previous, ok := latest[instruction.Key]; ok && previous == instruction.Text {
				instruction.Text = ""
				continue
			}
			latest[instruction.Key] = instruction.Text
		}
	}
	return input
}
