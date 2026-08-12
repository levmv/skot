package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/filesearch"
)

const (
	defaultReadLines    = 200
	maxReadLines        = 2000
	maxReadOutputBytes  = 256 * 1024
	maxReadableFileSize = 64 * 1024 * 1024
	maxEditableFileSize = 8 * 1024 * 1024
	maxGrepMatches      = 100
	maxGlobMatches      = 500
	maxSearchOutput     = 256 * 1024
)

func (workspace *workspace) read(ctx context.Context, raw string) (agent.ToolOutput, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.ToolOutput{}, err
	}
	abs, display, err := workspace.resolveReadableFile(args.Path)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	offset := args.Offset
	if offset <= 0 {
		offset = 1
	}
	limit := clampLimit(args.Limit, defaultReadLines, maxReadLines)
	content, next, more, digest, err := readNumberedLines(ctx, abs, offset, limit)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	var output strings.Builder
	fmt.Fprintf(&output, "sha256: %s\n", digest)
	if more {
		fmt.Fprintf(&output, "continue: read(path=%q, offset=%d, limit=%d)\n", display, next, limit)
	}
	output.WriteByte('\n')
	if content == "" {
		output.WriteString("no lines\n")
	} else {
		output.WriteString(content)
	}
	return agent.ToolOutput{Content: output.String()}, nil
}

func readNumberedLines(ctx context.Context, path string, offset, limit int) (content string, next int, more bool, digest string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return "", offset, false, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", offset, false, "", err
	}
	if info.Size() > maxReadableFileSize {
		return "", offset, false, "", fmt.Errorf("file exceeds %d-byte limit", maxReadableFileSize)
	}
	reader := bufio.NewReader(io.LimitReader(file, maxReadableFileSize+1))
	hash := sha256.New()
	var output strings.Builder
	lineNumber := 0
	returned := 0
	var bytesRead int64
	outputFull := false
	for {
		if err := ctx.Err(); err != nil {
			return "", offset, false, "", err
		}
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) > 0 {
			bytesRead += int64(len(raw))
			if bytesRead > maxReadableFileSize {
				return "", offset, false, "", fmt.Errorf("file exceeds %d-byte limit", maxReadableFileSize)
			}
			_, _ = hash.Write(raw)
			if bytes.IndexByte(raw, 0) >= 0 {
				return "", offset, false, "", errors.New("file appears to be binary")
			}
			if !utf8.Valid(raw) {
				return "", offset, false, "", errors.New("file is not valid UTF-8")
			}
			lineNumber++
			if lineNumber >= offset && returned < limit && !outputFull {
				line := raw
				if line[len(line)-1] == '\n' {
					line = line[:len(line)-1]
				}
				prefix := fmt.Sprintf("%6d\t", lineNumber)
				if output.Len()+len(prefix)+len(line)+1 <= maxReadOutputBytes {
					output.WriteString(prefix)
					_, _ = output.Write(line)
					output.WriteByte('\n')
					returned++
				} else if returned == 0 {
					output.WriteString(truncateNumberedLine(lineNumber, line))
					returned++
					outputFull = true
				} else {
					outputFull = true
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", offset, false, "", readErr
		}
	}
	digest = hex.EncodeToString(hash.Sum(nil))
	next = offset + returned
	more = next <= lineNumber
	return output.String(), next, more, digest, nil
}

func truncateNumberedLine(lineNumber int, line []byte) string {
	prefix := fmt.Sprintf("%6d\t", lineNumber)
	const suffix = "… [line truncated]\n"
	available := max(0, maxReadOutputBytes-len(prefix)-len(suffix))
	cut := min(len(line), available)
	for cut > 0 && !utf8.Valid(line[:cut]) {
		cut--
	}
	return prefix + string(line[:cut]) + suffix
}

func (workspace *workspace) grep(ctx context.Context, raw string) (agent.ToolOutput, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Include string `json:"include"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.ToolOutput{}, err
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return agent.ToolOutput{}, errors.New("pattern is required")
	}
	_, display, err := workspace.resolvePath(args.Path)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	lines, truncated, oversizedLines, err := workspace.runGrep(ctx, args.Pattern, args.Include, display)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	if len(lines) == 0 {
		if oversizedLines == 0 {
			return agent.ToolOutput{Content: "no matches\n"}, nil
		}
		return agent.ToolOutput{Content: fmt.Sprintf("no matches\nskipped_oversized_lines: %d\n", oversizedLines)}, nil
	}
	var output strings.Builder
	fmt.Fprintf(&output, "matches: %d\n", len(lines))
	if truncated {
		output.WriteString("truncated: true\n")
	}
	if oversizedLines > 0 {
		fmt.Fprintf(&output, "skipped_oversized_lines: %d\n", oversizedLines)
	}
	output.WriteByte('\n')
	output.WriteString(strings.Join(lines, "\n"))
	output.WriteByte('\n')
	return agent.ToolOutput{Content: output.String()}, nil
}

func (workspace *workspace) runGrep(ctx context.Context, pattern, include, display string) ([]string, bool, int, error) {
	query := filesearch.SearchQuery{Path: display, Pattern: pattern, PreviewBytes: 300}
	if strings.TrimSpace(include) != "" {
		query.Include = include
	}
	var lines []string
	usedBytes := 0
	stats, err := workspace.searcher.Search(ctx, query, func(match filesearch.Match) error {
		if len(lines) >= maxGrepMatches {
			return filesearch.ErrStop
		}
		text := match.Text
		if match.LineTruncated {
			text += " [... omitted end of long line]"
		}
		line := fmt.Sprintf("%s:%d:%s", formatSearchPath(match.Path, display), match.Line, text)
		if usedBytes+len(line)+1 > maxSearchOutput {
			return filesearch.ErrStop
		}
		lines = append(lines, line)
		usedBytes += len(line) + 1
		return nil
	})
	if err != nil {
		return nil, false, 0, err
	}
	return lines, stats.Stopped, stats.OversizedLinesSkipped, nil
}

func (workspace *workspace) glob(ctx context.Context, raw string) (agent.ToolOutput, error) {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.ToolOutput{}, err
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return agent.ToolOutput{}, errors.New("pattern is required")
	}
	_, display, err := workspace.resolvePath(args.Path)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	paths, truncated, err := workspace.runGlob(ctx, args.Pattern, display)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	if len(paths) == 0 {
		return agent.ToolOutput{Content: "no paths\n"}, nil
	}
	var output strings.Builder
	fmt.Fprintf(&output, "paths: %d\n", len(paths))
	if truncated {
		output.WriteString("truncated: true\n")
	}
	output.WriteByte('\n')
	output.WriteString(strings.Join(paths, "\n"))
	output.WriteByte('\n')
	return agent.ToolOutput{Content: output.String()}, nil
}

func (workspace *workspace) runGlob(ctx context.Context, pattern, display string) ([]string, bool, error) {
	var paths []string
	usedBytes := 0
	stats, err := workspace.searcher.Files(ctx, filesearch.FilesQuery{Path: display, Glob: pattern}, func(file filesearch.File) error {
		if len(paths) >= maxGlobMatches {
			return filesearch.ErrStop
		}
		path := formatSearchPath(file.Path, display)
		if usedBytes+len(path)+1 > maxSearchOutput {
			return filesearch.ErrStop
		}
		paths = append(paths, path)
		usedBytes += len(path) + 1
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return paths, stats.Stopped, nil
}

func formatSearchPath(path, queryPath string) string {
	if queryPath == "." {
		return "./" + path
	}
	return path
}

func (workspace *workspace) edit(ctx context.Context, raw string) (agent.ToolOutput, error) {
	var args struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.ToolOutput{}, err
	}
	if args.OldText == "" {
		return agent.ToolOutput{}, errors.New("old_text must not be empty")
	}
	if !utf8.ValidString(args.OldText) || !utf8.ValidString(args.NewText) {
		return agent.ToolOutput{}, errors.New("old_text and new_text must be valid UTF-8")
	}
	abs, display, err := workspace.resolveReadableFile(args.Path)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	old, err := readBoundedTextFile(abs, maxEditableFileSize)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	oldHash := sha256.Sum256(old)
	count := bytes.Count(old, []byte(args.OldText))
	if count != 1 {
		return agent.ToolOutput{}, fmt.Errorf("old_text occurs %d times in %s; reread and provide one exact occurrence", count, display)
	}
	if err := ctx.Err(); err != nil {
		return agent.ToolOutput{}, err
	}
	updated := bytes.Replace(old, []byte(args.OldText), []byte(args.NewText), 1)
	info, err := os.Stat(abs)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	if err := atomicReplace(abs, updated, info.Mode().Perm(), &oldHash, false); err != nil {
		return agent.ToolOutput{}, err
	}
	newHash := sha256.Sum256(updated)
	change := buildFileChangeMeta(display, "edited", old, updated)
	detail, err := fileChangeDetail(change)
	if err != nil {
		return agent.ToolOutput{}, fmt.Errorf("encode file change: %w", err)
	}
	return agent.ToolOutput{
		Content: fmt.Sprintf("updated\nsha256: %x", newHash),
		Details: []agent.Detail{detail},
	}, nil
}

func (workspace *workspace) write(ctx context.Context, raw string) (agent.ToolOutput, error) {
	var args struct {
		Path           string `json:"path"`
		Content        string `json:"content"`
		ExpectedSHA256 string `json:"expected_sha256"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.ToolOutput{}, err
	}
	if !utf8.ValidString(args.Content) {
		return agent.ToolOutput{}, errors.New("content must be valid UTF-8")
	}
	if len(args.Content) > maxEditableFileSize {
		return agent.ToolOutput{}, fmt.Errorf("content exceeds %d-byte write limit", maxEditableFileSize)
	}
	if err := ctx.Err(); err != nil {
		return agent.ToolOutput{}, err
	}
	target, err := workspace.resolveWriteTarget(args.Path)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	if args.ExpectedSHA256 != "" {
		if !target.exists {
			return agent.ToolOutput{}, errors.New("expected_sha256 was supplied but the file does not exist")
		}
		if err := verifyExpectedHash(args.ExpectedSHA256, target.digest); err != nil {
			return agent.ToolOutput{}, err
		}
	}
	updated := []byte(args.Content)
	var expected *[32]byte
	if target.exists {
		expected = &target.digest
	}
	if err := atomicReplace(target.abs, updated, target.mode, expected, !target.exists); err != nil {
		return agent.ToolOutput{}, err
	}
	newHash := sha256.Sum256(updated)
	operation := "created"
	if target.exists {
		operation = "replaced"
	}
	change := buildFileChangeMeta(target.display, operation, target.old, updated)
	detail, err := fileChangeDetail(change)
	if err != nil {
		return agent.ToolOutput{}, fmt.Errorf("encode file change: %w", err)
	}
	return agent.ToolOutput{
		Content: fmt.Sprintf("%s\nsha256: %x", operation, newHash),
		Details: []agent.Detail{detail},
	}, nil
}

type writeTarget struct {
	abs     string
	display string
	exists  bool
	mode    os.FileMode
	old     []byte
	digest  [32]byte
}

func (workspace *workspace) resolveWriteTarget(path string) (writeTarget, error) {
	abs, display, err := workspace.resolvePath(path)
	if err != nil {
		return writeTarget{}, err
	}
	if display == "." {
		return writeTarget{}, errors.New("file path is required")
	}
	if workspace.protected(abs) {
		return writeTarget{}, fmt.Errorf("path is protected: %s", display)
	}
	parent := filepath.Dir(abs)
	realParent, parentErr := ensureWritableParent(workspace.root, parent, workspace.protected)
	if parentErr != nil {
		return writeTarget{}, fmt.Errorf("prepare parent for %s: %w", display, parentErr)
	}
	abs = filepath.Join(realParent, filepath.Base(abs))
	if workspace.protected(abs) {
		return writeTarget{}, fmt.Errorf("path is protected: %s", display)
	}
	info, statErr := os.Lstat(abs)
	if errors.Is(statErr, os.ErrNotExist) {
		return writeTarget{abs: abs, display: display, mode: 0o644}, nil
	}
	if statErr != nil {
		return writeTarget{}, statErr
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return writeTarget{}, err
		}
		if !isWithinRoot(workspace.root, target) {
			return writeTarget{}, fmt.Errorf("symlink target escapes workspace root: %s", display)
		}
		if workspace.protected(target) {
			return writeTarget{}, fmt.Errorf("path is protected: %s", display)
		}
		abs = target
		info, err = os.Stat(abs)
		if err != nil {
			return writeTarget{}, err
		}
	}
	if !info.Mode().IsRegular() {
		return writeTarget{}, fmt.Errorf("%s is not a regular file", display)
	}
	old, err := readBoundedTextFile(abs, maxEditableFileSize)
	if err != nil {
		return writeTarget{}, err
	}
	return writeTarget{
		abs: abs, display: display, exists: true, mode: info.Mode().Perm(),
		old: old, digest: sha256.Sum256(old),
	}, nil
}

func ensureWritableParent(root, parent string, protected func(string) bool) (string, error) {
	probe := parent
	var missing []string
	for {
		info, err := os.Lstat(probe)
		if err == nil {
			if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				return "", fmt.Errorf("%s is not a directory", probe)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		missing = append(missing, filepath.Base(probe))
		next := filepath.Dir(probe)
		if next == probe {
			return "", errors.New("no existing parent directory")
		}
		probe = next
	}
	realParent, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return "", err
	}
	if !isWithinRoot(root, realParent) {
		return "", errors.New("parent symlink escapes workspace root")
	}
	if protected != nil && protected(realParent) {
		return "", errors.New("parent path is protected")
	}
	for index := len(missing) - 1; index >= 0; index-- {
		next := filepath.Join(realParent, missing[index])
		if protected != nil && protected(next) {
			return "", errors.New("parent path is protected")
		}
		if err := os.Mkdir(next, 0o755); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, statErr := os.Lstat(next)
			if statErr != nil {
				return "", statErr
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", errors.New("parent path changed to a symlink while creating directories")
			}
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", next)
			}
		}
		realParent = next
	}
	return realParent, nil
}

func atomicReplace(path string, content []byte, mode os.FileMode, expected *[32]byte, requireAbsent bool) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".sk-write-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if written, err := temporary.Write(content); err != nil || written != len(content) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if requireAbsent {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("file appeared while write was being prepared; reread before replacing it")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if expected != nil {
		current, err := readBoundedTextFile(path, maxEditableFileSize)
		if err != nil {
			return err
		}
		if currentHash := sha256.Sum256(current); currentHash != *expected {
			return errors.New("file changed while the update was being prepared; reread before retrying")
		}
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	if err := syncParentDir(directory); err != nil {
		return fmt.Errorf("sync file directory: %w", err)
	}
	ok = true
	return nil
}

func readBoundedTextFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds %d-byte limit", maxBytes)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("file appears to be binary")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("file is not valid UTF-8")
	}
	return data, nil
}

func verifyExpectedHash(raw string, actual [32]byte) error {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if len(raw) != 64 {
		return errors.New("expected_sha256 must contain 64 hexadecimal characters")
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		return errors.New("expected_sha256 must contain 64 hexadecimal characters")
	}
	if !bytes.Equal(decoded, actual[:]) {
		return fmt.Errorf("file hash is %s, not expected %s; reread before replacing", hex.EncodeToString(actual[:]), raw)
	}
	return nil
}
