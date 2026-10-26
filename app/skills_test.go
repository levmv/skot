package app

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/session"
	"github.com/levmv/skot/model"
	"github.com/levmv/skot/skills"
	workspacetools "github.com/levmv/skot/tools"
)

func TestQueuedSkillCapturesInstructionsBeforeDelivery(t *testing.T) {
	root := canonicalApplicationTestRoot(t, t.TempDir())
	home := canonicalApplicationTestRoot(t, t.TempDir())
	body := "Original instructions.\nCheck each proposed change.\n"
	path := writeApplicationSkill(t, filepath.Join(root, ".agents", "skills"), "review", body, "allowed-tools: [shell]\n")
	journal := &session.MemoryStore{}
	backend := &skillCaptureModel{}
	runtime, err := newApplicationTestRuntime(agent.Config{Backend: backend, Journal: journal, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	application := &Application{config: applicationConfig{root: root, home: home}, state: applicationState{session: newLiveSession("", runtime, journal, false)}}
	if err := application.QueueInput("$review check the changes"); err != nil {
		t.Fatal(err)
	}
	writeApplicationSkill(t, filepath.Join(root, ".agents", "skills"), "review", "New instructions.\n", "")
	prepared, ok := application.ClaimQueuedInput()
	if !ok || len(prepared.Instructions) != 1 || !strings.Contains(prepared.Instructions[0].Text, "Original instructions.") || strings.Contains(prepared.Instructions[0].Text, "New instructions.") {
		t.Fatalf("captured input = %#v, %v", prepared, ok)
	}
	if _, err := application.RunInput(t.Context(), prepared, nil); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(backend.requests))
	}
	delivered := backend.requests[0].Items[0].Text
	if !strings.Contains(delivered, body) || !strings.Contains(delivered, path) ||
		strings.Contains(delivered, "allowed-tools") || strings.Contains(delivered, "name: review") {
		t.Fatal("skill lost its body or source, or included frontmatter in the model input")
	}
}

func TestSkillReinvocationPreservesUpdatesAndDistinctSources(t *testing.T) {
	root := canonicalApplicationTestRoot(t, t.TempDir())
	home := canonicalApplicationTestRoot(t, t.TempDir())
	source := filepath.Join(root, ".agents", "skills")
	project := writeApplicationSkill(t, source, "review", "Original review instructions.\n", "")
	personal := writeApplicationSkill(t, filepath.Join(home, "skills"), "review", "Original review instructions.\n", "")
	writeApplicationSkill(t, source, "explain", "Explain the tradeoffs.\n", "")
	writeApplicationSkill(t, source, "verify", "Verify the result.\n", "")
	backend := &skillCaptureModel{}
	journal := &session.MemoryStore{}
	runtime, err := newApplicationTestRuntime(agent.Config{Backend: backend, Journal: journal, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	application := &Application{config: applicationConfig{root: root, home: home}, state: applicationState{session: newLiveSession("", runtime, journal, false)}}
	for _, step := range []struct {
		name       string
		body       string
		references []string
		wantBodies []string
	}{
		{"initial", "", []string{project, "explain"}, []string{"Original review instructions.", "Explain the tradeoffs."}},
		{"overlap", "", []string{project, "verify"}, []string{"Verify the result."}},
		{"unchanged", "", []string{project, "explain", "verify"}, nil},
		{"edited", "Revised review instructions.\n", []string{project}, []string{"Revised review instructions."}},
		{"reverted", "Original review instructions.\n", []string{project}, []string{"Original review instructions."}},
		{"different source", "", []string{personal}, []string{"Original review instructions."}},
	} {
		if step.body != "" {
			writeApplicationSkill(t, source, "review", step.body, "")
		}
		input, err := application.PrepareInput("apply the selected procedures", step.references...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := application.RunInput(t.Context(), input, nil); err != nil {
			t.Fatal(err)
		}
		request := backend.requests[len(backend.requests)-1]
		delivered := request.Items[len(request.Items)-1].Text
		for _, body := range []string{"Original review instructions.", "Revised review instructions.", "Explain the tradeoffs.", "Verify the result."} {
			if strings.Contains(delivered, body) != slices.Contains(step.wantBodies, body) {
				t.Fatalf("unexpected instructions for %s: %q", step.name, delivered)
			}
		}
		if step.name == "different source" && !strings.Contains(delivered, personal) {
			t.Fatal("distinct package lost its source directory")
		}
		state, err := runtime.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		item := state.Blocks[len(state.Blocks)-1].Entries[0].Item
		if !slices.Equal(agent.InputReferences(item), input.InstructionReferences()) {
			t.Fatalf("selected sources lost on replay for %s", step.name)
		}
		if step.name == "unchanged" {
			for _, reference := range input.InstructionReferences() {
				if !strings.Contains(delivered, reference.Name) || !strings.Contains(delivered, reference.Key) || reference.EstimatedTokens <= 0 {
					t.Fatal("repeat selection lost its name, source, or captured size")
				}
			}
			if !strings.Contains(delivered, "already in this conversation") {
				t.Fatal("repeat selection was not explained to the model")
			}
		}
	}
	if _, err := runtime.Run(t.Context(), "ordinary follow-up", nil); err != nil {
		t.Fatal(err)
	}
	request := backend.requests[len(backend.requests)-1]
	if request.Items[len(request.Items)-1].Text != "ordinary follow-up" {
		t.Fatal("ordinary follow-up repeated a skill selection")
	}
}

func TestDefaultProjectSkillsStayInsideWorkspace(t *testing.T) {
	root := canonicalApplicationTestRoot(t, t.TempDir())
	source := filepath.Join(root, ".agents", "skills")
	writeApplicationSkill(t, source, "обзор", "Review instructions.\n", "")
	application := &Application{config: applicationConfig{root: root, home: t.TempDir()}}
	if _, err := application.PrepareInput("$обзор проверь изменения"); err != nil {
		t.Fatal(err)
	}
	external := writeApplicationSkill(t, t.TempDir(), "external", "External instructions.", "")
	if err := os.Symlink(filepath.Dir(external), filepath.Join(source, "external")); err != nil {
		t.Fatal(err)
	}
	if _, err := application.PrepareInput("$external"); err == nil {
		t.Fatal("project package link bypassed the workspace boundary")
	}
	outside := filepath.Join(t.TempDir(), "skills")
	if err := os.Rename(source, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, source); err != nil {
		t.Fatal(err)
	}
	catalog := application.Skills()
	if len(catalog.Entries) != 0 || len(catalog.Diagnostics) != 1 || !errors.Is(catalog.Diagnostics[0].Err, os.ErrPermission) {
		t.Fatalf("external project source discovered: %#v", catalog)
	}
	// An explicitly configured source can live outside the workspace.
	application.config.skillSources = []skills.Source{{Path: source}}
	if _, err := application.PrepareInput("$обзор проверь изменения"); err != nil {
		t.Fatalf("explicit external source rejected: %v", err)
	}
}

func TestUserSkillResourcesFollowDeliveredSession(t *testing.T) {
	root := canonicalApplicationTestRoot(t, t.TempDir())
	home := canonicalApplicationTestRoot(t, t.TempDir())
	checkout := canonicalApplicationTestRoot(t, t.TempDir())
	manifest := writeApplicationSkill(t, checkout, "review", "Read reference.md for the procedure.\n", "")
	resources := filepath.Dir(manifest)
	reference := filepath.Join(resources, "reference.md")
	if err := os.WriteFile(reference, []byte("Captured package resources.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "skills"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(resources, filepath.Join(home, "skills", "review")); err != nil {
		t.Fatal(err)
	}
	fileTools, _, err := workspacetools.NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": reference})
	readResource := func(request model.Request) model.Response {
		if request.Items[len(request.Items)-1].Kind == model.ItemUserText {
			return model.Response{Items: []model.Item{{Kind: model.ItemToolCall, ToolCall: &model.ToolCall{Name: "read", RawArguments: string(args)}}}}
		}
		return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}, StopReason: "stop"}
	}
	backend := &skillCaptureModel{response: readResource}
	journal := &session.MemoryStore{}
	application := &Application{config: applicationConfig{root: root, home: home}}
	makeRuntime := func(journal agent.Journal) *agent.Runtime {
		t.Helper()
		runtime, err := newApplicationTestRuntime(agent.Config{
			Backend: backend, Journal: journal, Workspace: root, Tools: fileTools,
			AuthorizeReadOnlyPaths: application.authorizeSkillPaths,
		})
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	runtime := makeRuntime(journal)
	application.state.session = newLiveSession("", runtime, journal, false)
	if err := application.QueueInput("$review"); err != nil {
		t.Fatal(err)
	}
	input, ok := application.PopQueuedInput()
	if !ok || !slices.Equal(input.ReadOnlyPaths, []string{resources}) {
		t.Fatalf("prepared resources = %#v", input.ReadOnlyPaths)
	}
	assertRead := func(runtime *agent.Runtime, input agent.Input, wantError bool) {
		t.Helper()
		if _, err := runtime.RunInput(t.Context(), input, nil); err != nil {
			t.Fatal(err)
		}
		request := backend.requests[len(backend.requests)-1]
		result := request.Items[len(request.Items)-1].ToolResult
		if result == nil || result.Error != wantError || !wantError && !strings.Contains(result.Content.Text(), "Captured package resources.") {
			t.Fatalf("resource read = %#v, want error=%v", result, wantError)
		}
	}
	assertRead(runtime, agent.Input{Text: "before delivering the skill"}, true)
	if err := application.QueueUserInput(input); err != nil {
		t.Fatal(err)
	}
	assertRead(runtime, agent.Input{Text: "deliver queued skill"}, false)
	backend.response = nil
	for _, text := range []string{strings.Repeat("older conversation ", 8*1024), "recent question"} {
		if _, err := runtime.Run(t.Context(), text, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.Compact(t.Context()); err != nil {
		t.Fatal(err)
	}
	backend.response = readResource
	resumed := makeRuntime(journal)
	assertRead(resumed, agent.Input{Text: "resume with installed skill"}, false)
	assertRead(makeRuntime(&session.MemoryStore{}), agent.Input{Text: "new session"}, true)

	// A tampered journal cannot widen a package grant to its parent directory.
	records, err := journal.Records(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	forged := &session.MemoryStore{}
	for _, record := range records {
		if record.Kind == agent.RecordRunInputAdded {
			var input agent.RunInputAddedRecord
			if err := json.Unmarshal(record.Data, &input); err != nil {
				t.Fatal(err)
			}
			input.ReadOnlyPaths = []string{checkout}
			record.Data, err = json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := forged.Append(t.Context(), agent.PendingRecord{Kind: record.Kind, Data: record.Data}); err != nil {
			t.Fatal(err)
		}
	}
	assertRead(makeRuntime(forged), agent.Input{Text: "resume tampered journal"}, true)
	application.state.session = newLiveSession("", resumed, journal, false)
	if err := application.ClearSkills(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertRead(resumed, agent.Input{Text: "after clearing skills"}, true)
	reattached, err := application.PrepareInput("$review")
	if err != nil {
		t.Fatal(err)
	}
	assertRead(resumed, reattached, false)

	// Protecting the manifest revokes its resource grant, even though the
	// reference file itself is not protected by the file tool's policy.
	application.state.protection, err = workspacetools.NewProtectedPathPolicy(root, []string{manifest})
	if err != nil {
		t.Fatal(err)
	}
	assertRead(resumed, agent.Input{Text: "after protecting skill"}, true)
	application.state.protection = nil
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	assertRead(resumed, agent.Input{Text: "after removing skill"}, true)
	assertRead(makeRuntime(journal), agent.Input{Text: "resume without skill"}, true)
}

func TestSkillSelectionPrefersProjectAndAllowsExplicitUserPackage(t *testing.T) {
	root := canonicalApplicationTestRoot(t, t.TempDir())
	home := canonicalApplicationTestRoot(t, t.TempDir())
	project := writeApplicationSkill(t, filepath.Join(root, ".agents", "skills"), "review", "Project.\n", "")
	personal := writeApplicationSkill(t, filepath.Join(home, "skills"), "review", "Personal.\n", "")
	writeApplicationSkill(t, filepath.Join(home, "skills"), "hidden", "Hidden.\n", "user-invocable: false\n")
	application := &Application{config: applicationConfig{root: root, home: home}}
	for _, explicit := range []bool{false, true} {
		text := "$review check"
		var references []string
		if explicit {
			text, references = "check", []string{"review"}
		}
		prepared, err := application.PrepareInput(text, references...)
		if err != nil || len(prepared.Instructions) != 1 || prepared.Instructions[0].Key != project || !strings.Contains(prepared.Instructions[0].Text, "Project.") {
			t.Fatalf("project selection = %#v, %v", prepared, err)
		}
	}
	catalog := application.Skills()
	entry, err := catalog.Resolve(personal)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := application.PrepareInput(SkillReference(catalog, entry) + " check")
	if err != nil || len(prepared.Instructions) != 1 || prepared.Instructions[0].Key != personal || !strings.Contains(prepared.Instructions[0].Text, "Personal.") {
		t.Fatalf("exact selection = %#v, %v", prepared, err)
	}
	if _, err := application.PrepareInput("$hidden"); err == nil {
		t.Fatal("user-invocable restriction ignored")
	}
	application.state.protection, err = workspacetools.NewProtectedPathPolicy(root, []string{project})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := application.PrepareInput("check", project); err == nil {
		t.Fatal("protected skill loaded")
	}
	application.config.skillSources = []skills.Source{}
	if got := application.Skills(); len(got.Entries) != 0 || len(got.Diagnostics) != 0 {
		t.Fatalf("explicitly disabled sources = %#v", got)
	}
}

func TestSkillReferencesInProsePreserveLiteralExamples(t *testing.T) {
	for _, test := range []struct {
		text string
		refs []string
	}{
		{"$review check this", []string{"review"}},
		{"$обзор $数据-分析 проверь", []string{"обзор", "数据-分析"}},
		{"$review\n$explain\u2003check", []string{"review", "explain"}},
		{"check with $\"/a path/review/SKILL.md\".", []string{"/a path/review/SKILL.md"}},
		{"сделай ревью по $review.", []string{"review"}},
		{"($review), затем $обзор?", []string{"review", "обзор"}},
		{"use '$review'", []string{"review"}},
		{"`$review`", nil},
		{"`$example` then $review", []string{"review"}},
		{"```sh\n$example\n```\nuse $review", []string{"review"}},
		{"~~~sh\n$example\n~~~\nuse $review", []string{"review"}},
		{`literal \$review`, nil},
		{"!echo $review", nil},
		{"$HOME/path", nil},
		{"$(echo hello)", nil},
		{"$review.md", nil},
		{"$review_suffix", nil},
		{"word$review", nil},
		{"$\"/a/review/SKILL.md\"$review", nil},
	} {
		var refs []string
		for _, mention := range skillReferences(test.text) {
			if mention.reference != "" {
				refs = append(refs, mention.reference)
			}
		}
		if !slices.Equal(refs, test.refs) {
			t.Errorf("references(%q) = %q, want %q", test.text, refs, test.refs)
		}
	}
}

func TestUnknownDollarWordsAreTextButExplicitSkillErrorsAreReported(t *testing.T) {
	root := canonicalApplicationTestRoot(t, t.TempDir())
	source := filepath.Join(root, ".agents", "skills")
	writeApplicationSkill(t, source, "review", "Review instructions.\n", "")
	broken := writeApplicationSkill(t, source, "broken", "", "")
	if err := os.WriteFile(broken, []byte("invalid manifest"), 0600); err != nil {
		t.Fatal(err)
	}
	application := &Application{config: applicationConfig{root: root, home: t.TempDir()}}
	for _, text := range []string{"$5 за штуку — дорого?", "$foo не определена"} {
		input, err := application.PrepareInput(text)
		if err != nil || input.Text != text || len(input.Instructions) != 0 || len(input.ReadOnlyPaths) != 0 {
			t.Fatalf("ordinary input %q: %#v, %v", text, input, err)
		}
	}
	for _, text := range []string{"$review $foo is undefined", "проверь $foo через $review.", "use $review, then $foo and $review again"} {
		if input, err := application.PrepareInput(text); err != nil || input.Text != text || len(input.Instructions) != 1 || len(input.ReadOnlyPaths) != 1 {
			t.Fatalf("skill in prose %q: %#v, %v", text, input, err)
		}
	}
	for _, reference := range []string{"missing", "broken"} {
		if _, err := application.PrepareInput("check", reference); err == nil {
			t.Fatalf("explicit reference %q silently ignored", reference)
		}
	}
	if _, err := application.PrepareInput("check with $broken"); err == nil {
		t.Fatal("broken existing skill treated as ordinary text")
	}
}

func writeApplicationSkill(t *testing.T, source, name, body, fields string) string {
	t.Helper()
	path := filepath.Join(source, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\nname: "+name+"\ndescription: A reusable procedure.\n"+fields+"---\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type skillCaptureModel struct {
	requests []model.Request
	response func(model.Request) model.Response
}

func (backend *skillCaptureModel) Complete(_ context.Context, request model.Request, _ func(model.StreamEvent)) (model.Response, error) {
	backend.requests = append(backend.requests, request)
	if backend.response != nil {
		return backend.response(request), nil
	}
	return model.Response{Items: []model.Item{{Kind: model.ItemAssistantText, Text: "done"}}, StopReason: "stop"}, nil
}

func (*skillCaptureModel) ProjectModelItems(items []model.Item) []model.Item { return items }
