package app

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/levmv/skot/internal/canonicalpath"
	"github.com/levmv/skot/internal/state"
	workspacetools "github.com/levmv/skot/tools"
)

func openFilesystemPathsTestApplication(t *testing.T, config Config) *Application {
	t.Helper()
	if workspacetools.BoundaryBackend() == "" {
		t.Skip("platform sandbox is unavailable")
	}
	config.ModelURI, config.ModelExplicit, config.Interactive = "deepseek/deepseek-v4-flash", true, true
	application, err := Open(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	return application
}

func readWithFileTool(t *testing.T, application *Application, path string) error {
	t.Helper()
	for _, tool := range application.config.tools {
		if tool.Spec.Name != "read" {
			continue
		}
		arguments, err := json.Marshal(map[string]string{"path": path})
		if err != nil {
			t.Fatal(err)
		}
		_, runErr := tool.Run(context.Background(), string(arguments))
		return runErr
	}
	t.Fatal("read tool not found")
	return nil
}

func rememberedFilesystemPaths(t *testing.T, home, root string) state.WorkspaceSettings {
	t.Helper()
	preferences, err := state.OpenInteractive(home, canonicalApplicationTestRoot(t, root))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := preferences.Settings()
	if err != nil {
		t.Fatal(err)
	}
	return settings.Workspace
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAddedDirectoryAppliesLiveAndSurvivesInTheWorkspaceStore(t *testing.T) {
	home, root, outside := t.TempDir(), t.TempDir(), t.TempDir()
	shared := filepath.Join(outside, "shared.txt")
	writeTestFile(t, shared)
	application := openFilesystemPathsTestApplication(t, Config{Home: home, Root: root})

	if err := readWithFileTool(t, application, shared); err == nil || !strings.Contains(err.Error(), "outside workspace scope") {
		t.Fatalf("read before adding = %v", err)
	}
	if err := application.AddDirectory(context.Background(), outside); err != nil {
		t.Fatal(err)
	}
	if err := readWithFileTool(t, application, shared); err != nil {
		t.Fatalf("read after adding = %v", err)
	}
	want := canonicalpath.Resolve(outside)
	added, _ := application.FilesystemPaths()
	if len(added) != 1 || added[0].Path != want || added[0].Origin != FilesystemPathRemembered {
		t.Fatalf("filesystem paths = %#v", added)
	}
	if stored := rememberedFilesystemPaths(t, home, root); !slices.Equal(stored.AddedPaths, []string{want}) {
		t.Fatalf("stored added paths = %#v", stored.AddedPaths)
	}

	if err := application.RemoveAddedDirectory(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if err := readWithFileTool(t, application, shared); err == nil || !strings.Contains(err.Error(), "outside workspace scope") {
		t.Fatalf("read after removing = %v", err)
	}
	if added, _ = application.FilesystemPaths(); len(added) != 0 {
		t.Fatalf("filesystem paths after removing = %#v", added)
	}
	if stored := rememberedFilesystemPaths(t, home, root); len(stored.AddedPaths) != 0 {
		t.Fatalf("stored added paths after removing = %#v", stored.AddedPaths)
	}
}

func TestAddDirectoryRefusesPathsWhichWouldChangeNothing(t *testing.T) {
	home, root, outside := t.TempDir(), t.TempDir(), t.TempDir()
	nested := filepath.Join(outside, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	application := openFilesystemPathsTestApplication(t, Config{Home: home, Root: root})

	if err := application.AddDirectory(context.Background(), root); err == nil || !strings.Contains(err.Error(), "already inside the workspace") {
		t.Fatalf("adding the workspace itself = %v", err)
	}
	if err := application.AddDirectory(context.Background(), filepath.Join(outside, "missing")); err == nil {
		t.Fatal("adding a missing directory succeeded")
	}
	if err := application.AddDirectory(context.Background(), outside); err != nil {
		t.Fatal(err)
	}
	if err := application.AddDirectory(context.Background(), nested); err == nil || !strings.Contains(err.Error(), "already covered") {
		t.Fatalf("adding a nested directory = %v", err)
	}
	if stored := rememberedFilesystemPaths(t, home, root); len(stored.AddedPaths) != 1 {
		t.Fatalf("stored added paths = %#v", stored.AddedPaths)
	}
}

func TestRemoveAddedDirectoryRefusesInvocationPaths(t *testing.T) {
	home, root, outside := t.TempDir(), t.TempDir(), t.TempDir()
	shared := filepath.Join(outside, "shared.txt")
	writeTestFile(t, shared)
	application := openFilesystemPathsTestApplication(t, Config{Home: home, Root: root, AddedPaths: []string{outside}})

	added, _ := application.FilesystemPaths()
	if len(added) != 1 || added[0].Origin != FilesystemPathInvocation {
		t.Fatalf("filesystem paths = %#v", added)
	}
	err := application.RemoveAddedDirectory(context.Background(), outside)
	if err == nil || !strings.Contains(err.Error(), "-add-dir") {
		t.Fatalf("removing an invocation path = %v", err)
	}
	if err := readWithFileTool(t, application, shared); err != nil {
		t.Fatalf("read after the refused removal = %v", err)
	}
}

func TestProtectedPathAppliesLiveAndUnprotectRestoresAccess(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	secret := filepath.Join(root, ".env")
	writeTestFile(t, secret)
	writeTestFile(t, filepath.Join(root, "ordinary.txt"))
	application := openFilesystemPathsTestApplication(t, Config{Home: home, Root: root})

	assertSearchVisibility := func(visible bool) {
		t.Helper()
		searched := 0
		for _, tool := range application.config.tools {
			var arguments string
			switch tool.Spec.Name {
			case "grep":
				arguments = `{"pattern":"content","path":"."}`
			case "glob":
				arguments = `{"pattern":"*","path":"."}`
			default:
				continue
			}
			output, err := tool.Run(context.Background(), arguments)
			if err != nil {
				t.Fatalf("%s: %v", tool.Spec.Name, err)
			}
			text := output.Content.Text()
			if strings.Contains(text, ".env") != visible || !strings.Contains(text, "ordinary.txt") {
				t.Fatalf("%s with protected file visible=%t returned %q", tool.Spec.Name, visible, text)
			}
			searched++
		}
		if searched != 2 {
			t.Fatalf("tested %d search tools, want grep and glob", searched)
		}
	}

	assertSearchVisibility(true)
	if err := readWithFileTool(t, application, secret); err != nil {
		t.Fatalf("read before protecting = %v", err)
	}
	if err := application.ProtectPath(context.Background(), ".env"); err != nil {
		t.Fatal(err)
	}
	if err := readWithFileTool(t, application, secret); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("read after protecting = %v", err)
	}
	assertSearchVisibility(false)
	want := canonicalpath.Resolve(secret)
	_, protected := application.FilesystemPaths()
	if len(protected) != 1 || protected[0].Path != want || protected[0].Origin != FilesystemPathRemembered {
		t.Fatalf("filesystem paths = %#v", protected)
	}
	if stored := rememberedFilesystemPaths(t, home, root); !slices.Equal(stored.ProtectedPaths, []string{want}) {
		t.Fatalf("stored protected paths = %#v", stored.ProtectedPaths)
	}

	if err := application.UnprotectPath(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if err := readWithFileTool(t, application, secret); err != nil {
		t.Fatalf("read after unprotecting = %v", err)
	}
	assertSearchVisibility(true)
	if stored := rememberedFilesystemPaths(t, home, root); len(stored.ProtectedPaths) != 0 {
		t.Fatalf("stored protected paths after unprotecting = %#v", stored.ProtectedPaths)
	}
}

func TestProtectedPathsFromOtherLayersAreNotWeakenedFromASession(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"protected_paths":["settings-secret"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsSecret, invocationSecret := filepath.Join(root, "settings-secret"), filepath.Join(root, "api-secret")
	writeTestFile(t, settingsSecret)
	writeTestFile(t, invocationSecret)
	application := openFilesystemPathsTestApplication(t, Config{
		Home: home, Root: root, ProtectedPaths: []string{"api-secret"},
	})

	_, protected := application.FilesystemPaths()
	origins := make(map[string]FilesystemPathOrigin, len(protected))
	for _, path := range protected {
		origins[path.Path] = path.Origin
	}
	if origins[canonicalpath.Resolve(settingsSecret)] != FilesystemPathSettings ||
		origins[canonicalpath.Resolve(invocationSecret)] != FilesystemPathInvocation {
		t.Fatalf("protected path origins = %#v", protected)
	}
	if err := application.UnprotectPath(context.Background(), settingsSecret); err == nil || !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("unprotecting a settings path = %v", err)
	}
	if err := application.UnprotectPath(context.Background(), invocationSecret); err == nil || !strings.Contains(err.Error(), "-protect-path") {
		t.Fatalf("unprotecting an invocation path = %v", err)
	}
	if err := readWithFileTool(t, application, settingsSecret); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("read after the refused removals = %v", err)
	}
	if err := application.ProtectPath(context.Background(), filepath.Dir(root)); err == nil || !strings.Contains(err.Error(), "contains the workspace") {
		t.Fatalf("protecting the workspace parent = %v", err)
	}
}

func TestFilesystemPathRemovalUsesSelectedEntryAfterDirectoryChanges(t *testing.T) {
	for _, kind := range []string{"added", "protected"} {
		t.Run(kind, func(t *testing.T) {
			home, root := t.TempDir(), t.TempDir()
			parent := root
			if kind == "added" {
				parent = t.TempDir()
			}
			first, second := filepath.Join(parent, "first"), filepath.Join(parent, "second")
			for _, directory := range []string{first, second} {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			application := openFilesystemPathsTestApplication(t, Config{Home: home, Root: root})
			add, remove := application.AddDirectory, application.RemoveAddedDirectory
			if kind == "protected" {
				add, remove = application.ProtectPath, application.UnprotectPath
			}
			for _, directory := range []string{first, second} {
				if err := add(context.Background(), directory); err != nil {
					t.Fatal(err)
				}
			}
			entries, protected := application.FilesystemPaths()
			if kind == "protected" {
				entries = protected
			}
			if len(entries) != 2 {
				t.Fatalf("filesystem paths before removal = %#v", entries)
			}
			selected, retained := entries[0], entries[1]
			if err := os.Rename(selected.Path, selected.Path+"-previous"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(retained.Path, selected.Path); err != nil {
				t.Fatal(err)
			}

			if err := remove(context.Background(), selected.Path); err != nil {
				t.Fatal(err)
			}
			entries, protected = application.FilesystemPaths()
			if kind == "protected" {
				entries = protected
			}
			if !slices.Equal(entries, []FilesystemPath{retained}) {
				t.Fatalf("filesystem paths after removal = %#v; want %#v", entries, retained)
			}
			settings := rememberedFilesystemPaths(t, home, root)
			stored := settings.AddedPaths
			if kind == "protected" {
				stored = settings.ProtectedPaths
			}
			if !slices.Equal(stored, []string{retained.Path}) {
				t.Fatalf("remembered paths after removal = %#v; want %q", stored, retained.Path)
			}
		})
	}
}

func TestRememberedPathsComeBackOnTheNextStart(t *testing.T) {
	home, root, outside := t.TempDir(), t.TempDir(), t.TempDir()
	shared := filepath.Join(outside, "shared.txt")
	writeTestFile(t, shared)
	secret := filepath.Join(root, ".env")
	writeTestFile(t, secret)

	first := openFilesystemPathsTestApplication(t, Config{Home: home, Root: root})
	if err := first.AddDirectory(context.Background(), outside); err != nil {
		t.Fatal(err)
	}
	if err := first.ProtectPath(context.Background(), ".env"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// A second run reads the same workspace record, so the reach the user chose
	// is the reach the model gets.
	second := openFilesystemPathsTestApplication(t, Config{Home: home, Root: root})
	added, protected := second.FilesystemPaths()
	if len(added) != 1 || added[0].Path != canonicalpath.Resolve(outside) || added[0].Origin != FilesystemPathRemembered {
		t.Fatalf("added paths after restart = %#v", added)
	}
	if len(protected) != 1 || protected[0].Path != canonicalpath.Resolve(secret) || protected[0].Origin != FilesystemPathRemembered {
		t.Fatalf("protected paths after restart = %#v", protected)
	}
	if err := readWithFileTool(t, second, shared); err != nil {
		t.Fatalf("read of a remembered added directory = %v", err)
	}
	if err := readWithFileTool(t, second, secret); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("read of a remembered protected path = %v", err)
	}
}
