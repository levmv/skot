package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/canonicalpath"
)

func TestWorkspaceToolsExposeStandaloneCatalog(t *testing.T) {
	tools, root, err := NewWorkspaceTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(root) {
		t.Fatalf("root = %q", root)
	}
	var names []string
	for index, tool := range tools {
		names = append(names, tool.Spec.Name)
		if len(tool.Spec.InputSchema) == 0 || tool.Run == nil {
			t.Fatalf("invalid tool %#v", tool.Spec)
		}
		if (index < 4) != tool.Spec.ParallelSafe {
			t.Fatalf("tool policy = %#v", tool.Spec)
		}
	}
	if got, want := strings.Join(names, ","), "read,ls,grep,glob,edit,write"; got != want {
		t.Fatalf("tools = %q, want %q", got, want)
	}
	if _, err := agent.New(agent.Config{
		Model:   agent.ModelInfo{BackendID: "test", Provider: "test", Model: "test"},
		Backend: inertModel{}, Journal: inertJournal{}, Tools: tools,
	}); err != nil {
		t.Fatalf("agent rejected tool catalog: %v", err)
	}
}

func TestSharedFilesystemConstructorsRejectUninitializedAccess(t *testing.T) {
	access := &FilesystemAccess{}
	if _, _, err := NewWorkspaceToolsWithAccess(access); err == nil || !strings.Contains(err.Error(), "uninitialized") {
		t.Fatalf("workspace tools error = %v", err)
	}
	if _, err := NewProcessManagerWithAccess(access, t.TempDir(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "uninitialized") {
		t.Fatalf("process manager error = %v", err)
	}
}

func TestReadAndLSReturnBoundedStructuredText(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "docs", "note.txt"), "one\ntwo\nthree\n")
	mustWriteFile(t, filepath.Join(root, "two words.txt"), "spaced\n")
	if err := os.Symlink("docs", filepath.Join(root, "current")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tools, _, err := NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	read := mustRunTool(t, tools, "read", `{"path":"docs/note.txt","offset":2,"limit":1}`)
	if !strings.Contains(read, "     2\ttwo") || !strings.Contains(read, "offset=3") ||
		!strings.Contains(read, "truncated: true") ||
		!regexp.MustCompile(`(?m)^sha256: [0-9a-f]{64}$`).MatchString(read) {
		t.Fatalf("read result = %q", read)
	}
	list := mustRunTool(t, tools, "ls", `{}`)
	if !strings.Contains(list, "dir\tdocs/") || !strings.Contains(list, "symlink\tcurrent -> docs") ||
		!strings.Contains(list, "file\t\"two words.txt\"") {
		t.Fatalf("ls result = %q", list)
	}
}

func TestReadReturnsNormalizedPNGAndJPEGContent(t *testing.T) {
	for _, test := range []struct {
		name, extension, mediaType, decodedFormat string
		encode                                    func(*os.File, image.Image) error
	}{
		{name: "PNG", extension: "png", mediaType: "image/png", decodedFormat: "png", encode: func(file *os.File, source image.Image) error {
			return png.Encode(file, source)
		}},
		{name: "JPEG", extension: "data", mediaType: "image/jpeg", decodedFormat: "jpeg", encode: func(file *os.File, source image.Image) error {
			return jpeg.Encode(file, source, nil)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "screens", "wide."+test.extension)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.encode(file, image.NewNRGBA(image.Rect(0, 0, 2400, 12))); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}

			workspaceTools, _, err := NewWorkspaceTools(root)
			if err != nil {
				t.Fatal(err)
			}
			output, err := runTool(workspaceTools, "read", fmt.Sprintf(`{"path":"screens/wide.%s","offset":99,"limit":1}`, test.extension))
			if err != nil {
				t.Fatal(err)
			}
			if !output.Content.HasImage() || len(output.Content) != 2 || strings.TrimSpace(output.Content.Text()) == "" {
				t.Fatalf("read image content = %#v", output.Content)
			}
			imagePart := output.Content[1].Image
			if imagePart == nil || imagePart.MediaType != test.mediaType || imagePart.Width != 2000 || imagePart.Height != 10 || len(imagePart.Data) == 0 {
				t.Fatalf("normalized image = %#v", imagePart)
			}
			decoded, format, err := image.Decode(bytes.NewReader(imagePart.Data))
			if err != nil {
				t.Fatal(err)
			}
			if format != test.decodedFormat || decoded.Bounds().Dx() != 2000 || decoded.Bounds().Dy() != 10 {
				t.Fatalf("decode normalized image: format=%q bounds=%v", format, decoded.Bounds())
			}
		})
	}
}

func TestReadPreservesSuitableJPEGBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "small.jpeg")
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 32, 16)), &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	workspaceTools, _, err := NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	output, err := runTool(workspaceTools, "read", `{"path":"small.jpeg"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Content) != 2 || output.Content[1].Image == nil || !bytes.Equal(output.Content[1].Image.Data, encoded.Bytes()) {
		t.Fatalf("suitable JPEG was needlessly re-encoded: %#v", output.Content)
	}
}

func TestReadUsesFirstGIFFrame(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "animated.gif")
	palette := color.Palette{color.Transparent, color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}}
	first := image.NewPaletted(image.Rect(0, 0, 4, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 4, 2), palette)
	for index := range first.Pix {
		first.Pix[index] = 1
		second.Pix[index] = 2
	}
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	workspaceTools, _, err := NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	output, err := runTool(workspaceTools, "read", `{"path":"animated.gif"}`)
	if err != nil {
		t.Fatal(err)
	}
	imagePart := output.Content[1].Image
	if imagePart == nil || imagePart.MediaType != "image/png" {
		t.Fatalf("GIF content = %#v", output.Content)
	}
	decoded, format, err := image.Decode(bytes.NewReader(imagePart.Data))
	if err != nil {
		t.Fatal(err)
	}
	red, _, blue, _ := decoded.At(0, 0).RGBA()
	if format != "png" || red <= blue {
		t.Fatalf("GIF first frame was not preserved: format=%q rgba=%d,_,%d,_", format, red, blue)
	}
}

func TestReadConvertsWebPToJPEG(t *testing.T) {
	const fixture = "UklGRrIBAABXRUJQVlA4TKUBAAAvSsAYAA8w//M///MfeJAkbXvaSG7m8Q3GfYSBJekwQztm/IcZlgwnmWImn2BK7aFmBtnVir6q//8VOkFE/xm4baTIu8c48ArEo6+B3zFKYln3pqClSCKX0begFTAXFOLXHSyF8cCNcZEG4OywuA4KVVfJCiArU7GAgJI8+lJP/OKMT/fBAjevg1cYB7YVkFuWga2lyPi5I0HFy5YTpWIHg0RZpkniRVW9odHAKOwosWuOGdxIyn2OvaCDvhg/we6TwadPBPbqBV58MsLmMJ8yZnOWk8SRz4N+QoyPL+MnamzMvcE1rHNEr91F9GKZPVUcS9w7PhhH36suB9qPeYb/oLk6cuTiJ0wOK3m5h1cKjW6EVZCYMK7dxcKCBdgP9HkKr9gkAO2P8GKZGWVdIAatQa+1IDpt6qyorVwdy01xdW8Jkfk6xjEXmVQQ+HQdFr6OKhIN34dXWq0+0qr6EJSCeeVLH9+gvGTLyqM65PQ44ihzlTXxQKjKbAvshXgir7Lil9w4L2bvMycmjQcqXaMCO6BlY28i+FOLzbfI1vEqxAhotocAAA=="
	data, err := base64.StdEncoding.DecodeString(fixture)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "image.webp"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	workspaceTools, _, err := NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	output, err := runTool(workspaceTools, "read", `{"path":"image.webp"}`)
	if err != nil {
		t.Fatal(err)
	}
	imagePart := output.Content[1].Image
	if imagePart == nil || imagePart.MediaType != "image/jpeg" {
		t.Fatalf("WebP content = %#v", output.Content)
	}
	if _, format, err := image.Decode(bytes.NewReader(imagePart.Data)); err != nil || format != "jpeg" {
		t.Fatalf("decode converted WebP: format=%q error=%v", format, err)
	}
}

func TestJPEGNormalizationCompositesTransparencyOnWhite(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 64, 16))
	for y := range 16 {
		for x := 32; x < 64; x++ {
			source.Set(x, y, color.Black)
		}
	}
	data, _, _, err := normalizeImage(context.Background(), source, "jpeg", 64, 16)
	if err != nil {
		t.Fatal(err)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	whiteR, whiteG, whiteB, _ := decoded.At(4, 8).RGBA()
	blackR, blackG, blackB, _ := decoded.At(60, 8).RGBA()
	if whiteR < 0xe000 || whiteG < 0xe000 || whiteB < 0xe000 || blackR > 0x2000 || blackG > 0x2000 || blackB > 0x2000 {
		t.Fatalf("JPEG matte colors = white(%x,%x,%x) black(%x,%x,%x)", whiteR, whiteG, whiteB, blackR, blackG, blackB)
	}
}

func TestReadRejectsCorruptImage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "broken.png")
	if err := os.WriteFile(path, append([]byte("\x89PNG\r\n\x1a\n"), []byte("broken")...), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(tools, "read", `{"path":"broken.png"}`); err == nil {
		t.Fatalf("corrupt image error = %v", err)
	}
}

func TestSearchToolsHonorIgnoresAndNeedNoExternalBinary(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "secret.go"), "needle\n")
	mustWriteFile(t, filepath.Join(root, ".config", "settings.go"), "needle\n")
	mustWriteFile(t, filepath.Join(root, "ignored", "inside.go"), "needle\n")
	mustWriteFile(t, filepath.Join(root, ".gitignore"), "ignored/\n")
	mustWriteFile(t, filepath.Join(root, "pkg", "visible.go"), "needle\n")
	tools, _, err := NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	grep := mustRunTool(t, tools, "grep", `{"pattern":"needle","include":"*.go"}`)
	if !strings.Contains(grep, "pkg/visible.go") || !strings.Contains(grep, ".config/settings.go") ||
		strings.Contains(grep, ".git/") || strings.Contains(grep, "ignored/") {
		t.Fatalf("grep result = %q", grep)
	}
	glob := mustRunTool(t, tools, "glob", `{"pattern":"**/*.go"}`)
	if !strings.Contains(glob, "pkg/visible.go") || !strings.Contains(glob, ".config/settings.go") ||
		!strings.Contains(glob, "ignored/inside.go") || strings.Contains(glob, ".git/") {
		t.Fatalf("glob result = %q", glob)
	}
}

func TestWorkspaceToolsConfineReadsAndWrites(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "secret.txt"), "secret\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	access, err := NewFilesystemAccess(root, ScopeWorkspace, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := newWorkspaceWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	relativeOutside, err := filepath.Rel(root, filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.read(context.Background(), jsonArgs(t, map[string]any{"path": relativeOutside})); err == nil {
		t.Fatal("read accepted parent traversal outside the workspace")
	}
	if _, err := workspace.read(context.Background(), `{"path":"escape/secret.txt"}`); err == nil {
		t.Fatal("read through escaping symlink was accepted")
	}
	if _, err := workspace.write(context.Background(), `{"path":"escape/new/file.txt","content":"bad"}`); err == nil {
		t.Fatal("write through escaping symlink was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside directory was created: %v", err)
	}
}

func TestMachineScopeFileToolsReachExplicitExternalPaths(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	external := filepath.Join(outside, "external.txt")
	mustWriteFile(t, inside, "workspace only\n")
	mustWriteFile(t, external, "external needle\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	access, err := NewFilesystemAccess(root, ScopeMachine, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	canonicalExternal := filepath.ToSlash(canonicalpath.Resolve(external))

	read := mustRunTool(t, tools, "read", jsonArgs(t, map[string]any{"path": external}))
	if !strings.Contains(read, "external needle") {
		t.Fatalf("external read = %q", read)
	}
	symlinkRead := mustRunTool(t, tools, "read", `{"path":"escape/external.txt"}`)
	if !strings.Contains(symlinkRead, "external needle") {
		t.Fatalf("external symlink read = %q", symlinkRead)
	}
	relativeExternal, err := filepath.Rel(root, external)
	if err != nil {
		t.Fatal(err)
	}
	if read := mustRunTool(t, tools, "read", jsonArgs(t, map[string]any{"path": relativeExternal})); !strings.Contains(read, "external needle") {
		t.Fatalf("relative escape read = %q", read)
	}
	list := mustRunTool(t, tools, "ls", jsonArgs(t, map[string]any{"path": outside}))
	if !strings.Contains(list, "external.txt") {
		t.Fatalf("external ls = %q", list)
	}
	grep := mustRunTool(t, tools, "grep", jsonArgs(t, map[string]any{"pattern": "needle", "path": outside}))
	if !strings.Contains(grep, canonicalExternal+":1:external needle") {
		t.Fatalf("external grep = %q", grep)
	}
	grepFile := mustRunTool(t, tools, "grep", jsonArgs(t, map[string]any{"pattern": "needle", "path": external}))
	if !strings.Contains(grepFile, canonicalExternal+":1:external needle") {
		t.Fatalf("external-file grep = %q", grepFile)
	}
	glob := mustRunTool(t, tools, "glob", jsonArgs(t, map[string]any{"pattern": "*.txt", "path": outside}))
	if !strings.Contains(glob, canonicalExternal) {
		t.Fatalf("external glob = %q", glob)
	}
	if omitted := mustRunTool(t, tools, "grep", `{"pattern":"external needle"}`); omitted != "no matches\n" {
		t.Fatalf("omitted grep left workspace = %q", omitted)
	}

	edit, err := runTool(tools, "edit", jsonArgs(t, map[string]any{
		"path": external, "old_text": "external", "new_text": "changed",
	}))
	if err != nil {
		t.Fatal(err)
	}
	change, ok := agent.FileChangeFromDetail(edit.Details[0])
	if !ok || change.Path != canonicalExternal {
		t.Fatalf("external edit detail = %#v, ok=%v", change, ok)
	}
	created := filepath.Join(outside, "nested", "created.txt")
	write, err := runTool(tools, "write", jsonArgs(t, map[string]any{"path": created, "content": "created\n"}))
	if err != nil {
		t.Fatal(err)
	}
	change, ok = agent.FileChangeFromDetail(write.Details[0])
	if !ok || change.Path != filepath.ToSlash(canonicalpath.Resolve(created)) {
		t.Fatalf("external write detail = %#v, ok=%v", change, ok)
	}
	throughAlias := filepath.Join(outside, "through-alias.txt")
	write, err = runTool(tools, "write", `{"path":"escape/through-alias.txt","content":"alias\n"}`)
	if err != nil {
		t.Fatal(err)
	}
	change, ok = agent.FileChangeFromDetail(write.Details[0])
	if !ok || change.Path != filepath.ToSlash(canonicalpath.Resolve(throughAlias)) {
		t.Fatalf("external alias write detail = %#v, ok=%v", change, ok)
	}
}

func TestMachineScopeSearchesThroughExternalAliasIntoWorkspace(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	mustWriteFile(t, inside, "workspace backlink\n")
	alias := filepath.Join(outside, "workspace-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	access, err := NewFilesystemAccess(root, ScopeMachine, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	grep := mustRunTool(t, tools, "grep", jsonArgs(t, map[string]any{
		"pattern": "backlink", "path": alias,
	}))
	want := filepath.ToSlash(canonicalpath.Resolve(inside)) + ":1:workspace backlink"
	if !strings.Contains(grep, want) {
		t.Fatalf("external-alias grep = %q; want %q", grep, want)
	}
}

func TestWorkspaceScopeAcceptsAbsoluteInsideAndRejectsExternalPaths(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	external := filepath.Join(outside, "external.txt")
	mustWriteFile(t, inside, "inside\n")
	mustWriteFile(t, external, "outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	workspaceAlias := filepath.Join(outside, "workspace-alias")
	if err := os.Symlink(root, workspaceAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	access, err := NewFilesystemAccess(root, ScopeWorkspace, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	if read := mustRunTool(t, tools, "read", jsonArgs(t, map[string]any{"path": inside})); !strings.Contains(read, "inside") {
		t.Fatalf("absolute in-workspace read = %q", read)
	}
	if read := mustRunTool(t, tools, "read", jsonArgs(t, map[string]any{"path": filepath.Join(workspaceAlias, "inside.txt")})); !strings.Contains(read, "inside") {
		t.Fatalf("absolute workspace-alias read = %q", read)
	}
	createdInside := filepath.Join(canonicalpath.Resolve(root), "created.txt")
	write, err := runTool(tools, "write", jsonArgs(t, map[string]any{"path": createdInside, "content": "inside\n"}))
	if err != nil {
		t.Fatal(err)
	}
	change, ok := agent.FileChangeFromDetail(write.Details[0])
	if !ok || change.Path != "created.txt" {
		t.Fatalf("absolute in-workspace write detail = %#v, ok=%v", change, ok)
	}

	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{name: "read", args: map[string]any{"path": external}},
		{name: "read", args: map[string]any{"path": "escape/external.txt"}},
		{name: "ls", args: map[string]any{"path": outside}},
		{name: "grep", args: map[string]any{"pattern": "outside", "path": outside}},
		{name: "glob", args: map[string]any{"pattern": "*.txt", "path": outside}},
		{name: "edit", args: map[string]any{"path": external, "old_text": "outside", "new_text": "changed"}},
		{name: "write", args: map[string]any{"path": filepath.Join(outside, "new.txt"), "content": "bad"}},
	} {
		if _, err := runTool(tools, test.name, jsonArgs(t, test.args)); err == nil || !strings.Contains(err.Error(), "scope") {
			t.Fatalf("%s external error = %v", test.name, err)
		}
	}
}

func TestWorkspaceScopeRejectsSiblingPathWithSharedPrefix(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	siblingFile := filepath.Join(parent, "workspace-other", "sibling.txt")
	mustWriteFile(t, filepath.Join(root, "inside.txt"), "inside\n")
	mustWriteFile(t, siblingFile, "sibling\n")
	access, err := NewFilesystemAccess(root, ScopeWorkspace, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(tools, "read", jsonArgs(t, map[string]any{"path": siblingFile})); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("sibling-prefix read error = %v", err)
	}
}

func TestSharedFilesystemAccessSwitchesFileTools(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	external := filepath.Join(outside, "external.txt")
	mustWriteFile(t, external, "external\n")
	access, err := NewFilesystemAccess(root, ScopeMachine, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManagerWithAccess(access, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if _, err := runTool(tools, "read", jsonArgs(t, map[string]any{"path": external})); err != nil {
		t.Fatalf("machine read: %v", err)
	}
	if err := setScopeAfter(manager, ScopeWorkspace, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(tools, "read", jsonArgs(t, map[string]any{"path": external})); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("file tools did not observe switched scope: %v", err)
	}
}

func TestMachineScopeStillFiltersExternalProtectedPaths(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	secretDir := filepath.Join(outside, "private")
	secret := filepath.Join(secretDir, "secret.txt")
	public := filepath.Join(outside, "public.txt")
	mustWriteFile(t, secret, "needle secret\n")
	mustWriteFile(t, public, "needle public\n")
	protection, err := NewProtectedPathPolicy(root, []string{secretDir})
	if err != nil {
		t.Fatal(err)
	}
	access, err := NewFilesystemAccess(root, ScopeMachine, nil, protection)
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithAccess(access)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(tools, "read", jsonArgs(t, map[string]any{"path": secret})); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected external read error = %v", err)
	}
	if _, err := runTool(tools, "write", jsonArgs(t, map[string]any{
		"path": filepath.Join(secretDir, "missing", "new.txt"), "content": "secret",
	})); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("protected external write error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(secretDir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("protected write created parent: %v", err)
	}
	list := mustRunTool(t, tools, "ls", jsonArgs(t, map[string]any{"path": outside}))
	if !strings.Contains(list, "public.txt") || strings.Contains(list, "private") {
		t.Fatalf("external protected ls = %q", list)
	}
	canonicalPublic := filepath.ToSlash(canonicalpath.Resolve(public))
	grep := mustRunTool(t, tools, "grep", jsonArgs(t, map[string]any{"pattern": "needle", "path": outside}))
	if !strings.Contains(grep, canonicalPublic) || strings.Contains(grep, "secret.txt") {
		t.Fatalf("external protected grep = %q", grep)
	}
	glob := mustRunTool(t, tools, "glob", jsonArgs(t, map[string]any{"pattern": "**/*.txt", "path": outside}))
	if !strings.Contains(glob, canonicalPublic) || strings.Contains(glob, "secret.txt") {
		t.Fatalf("external protected glob = %q", glob)
	}
}

func TestWorkspaceToolsHideProtectedPathsAndAliases(t *testing.T) {
	root := t.TempDir()
	protectedDir := filepath.Join(root, "private")
	mustWriteFile(t, filepath.Join(protectedDir, "secret.txt"), "needle secret\n")
	mustWriteFile(t, filepath.Join(root, "public.txt"), "needle public\n")
	if err := os.Symlink("private", filepath.Join(root, "private-alias")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	policy, err := NewProtectedPathPolicy(root, []string{"private"})
	if err != nil {
		t.Fatal(err)
	}
	tools, _, err := NewWorkspaceToolsWithProtection(root, policy)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		args string
	}{
		{name: "read", args: `{"path":"private/secret.txt"}`},
		{name: "read", args: `{"path":"private-alias/secret.txt"}`},
		{name: "edit", args: `{"path":"private/secret.txt","old_text":"secret","new_text":"changed"}`},
		{name: "write", args: `{"path":"private/new/note.txt","content":"changed"}`},
	} {
		if _, err := runTool(tools, test.name, test.args); err == nil || !strings.Contains(err.Error(), "protected") {
			t.Fatalf("%s(%s) error = %v", test.name, test.args, err)
		}
	}
	if _, err := os.Stat(filepath.Join(protectedDir, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write created a protected parent: %v", err)
	}
	list := mustRunTool(t, tools, "ls", `{}`)
	if !strings.Contains(list, "public.txt") || strings.Contains(list, "private") {
		t.Fatalf("ls result = %q", list)
	}
	grep := mustRunTool(t, tools, "grep", `{"pattern":"needle"}`)
	if !strings.Contains(grep, "public.txt") || strings.Contains(grep, "secret.txt") {
		t.Fatalf("grep result = %q", grep)
	}
	glob := mustRunTool(t, tools, "glob", `{"pattern":"**/*.txt"}`)
	if !strings.Contains(glob, "public.txt") || strings.Contains(glob, "secret.txt") {
		t.Fatalf("glob result = %q", glob)
	}

}

func TestEditAndWriteUseHashesAndAtomicReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.txt")
	mustWriteFile(t, path, "alpha\nbeta\n")
	tools, _, err := NewWorkspaceTools(root)
	if err != nil {
		t.Fatal(err)
	}
	read := mustRunTool(t, tools, "read", `{"path":"note.txt"}`)
	digest := regexp.MustCompile(`(?m)^sha256: ([0-9a-f]{64})$`).FindStringSubmatch(read)
	if len(digest) != 2 {
		t.Fatalf("read digest missing: %q", read)
	}
	editOutput, err := runTool(tools, "edit", `{"path":"note.txt","old_text":"beta","new_text":"gamma"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^operation: edited\nsha256: [0-9a-f]{64}\n$`).MatchString(editOutput.Content.Text()) {
		t.Fatalf("edit result = %q", editOutput.Content.Text())
	}
	if len(editOutput.Details) != 1 {
		t.Fatalf("edit details = %#v", editOutput.Details)
	}
	editChange, ok := agent.FileChangeFromDetail(editOutput.Details[0])
	if !ok || editChange.Path != "note.txt" || editChange.Operation != "edited" || editChange.Additions != 1 || editChange.Deletions != 1 {
		t.Fatalf("edit change = %#v, ok=%v", editChange, ok)
	}
	if _, err := runTool(tools, "write", `{"path":"note.txt","content":"stale","expected_sha256":"`+digest[1]+`"}`); err == nil {
		t.Fatal("write accepted a stale hash")
	}
	writeOutput, err := runTool(tools, "write", `{"path":"new/nested.txt","content":"hello\n"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^operation: created\nsha256: [0-9a-f]{64}\n$`).MatchString(writeOutput.Content.Text()) {
		t.Fatalf("write result = %q", writeOutput.Content.Text())
	}
	if len(writeOutput.Details) != 1 {
		t.Fatalf("write details = %#v", writeOutput.Details)
	}
	writeChange, ok := agent.FileChangeFromDetail(writeOutput.Details[0])
	if !ok || writeChange.Path != "new/nested.txt" || writeChange.Operation != "created" || writeChange.Additions != 1 || writeChange.Deletions != 0 {
		t.Fatalf("write change = %#v, ok=%v", writeChange, ok)
	}
	data, err := os.ReadFile(filepath.Join(root, "new", "nested.txt"))
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("written file = %q, error = %v", data, err)
	}
}

func TestToolArgumentsRejectUnknownFields(t *testing.T) {
	tools, _, err := NewWorkspaceTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runTool(tools, "ls", `{"surprise":true}`); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
}

func runTool(tools []agent.Tool, name, arguments string) (agent.ToolOutput, error) {
	for _, tool := range tools {
		if tool.Spec.Name == name {
			return tool.Run(context.Background(), arguments)
		}
	}
	return agent.ToolOutput{}, errors.New("tool not found")
}

func mustRunTool(t *testing.T, tools []agent.Tool, name, arguments string) string {
	t.Helper()
	result, err := runTool(tools, name, arguments)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result.Content.Text()
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsonArgs(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type inertModel struct{}

func (inertModel) Complete(context.Context, agent.ModelRequest, func(agent.ModelStreamEvent)) (agent.ModelResponse, error) {
	return agent.ModelResponse{}, errors.New("unused")
}

type inertJournal struct{}

func (inertJournal) Append(context.Context, agent.PendingRecord) (agent.Record, error) {
	return agent.Record{}, errors.New("unused")
}

func (inertJournal) Records(context.Context) ([]agent.Record, error) { return nil, nil }

func (inertModel) ProjectModelItems(items []agent.Item) []agent.Item { return items }
