package filesearch

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	initialIgnoreBufferBytes = 4 * 1024
	maxIgnoreFileBytes       = 8 * 1024 * 1024
	maxIgnorePatterns        = 64 * 1024
)

var errIgnorePatternTooLong = errors.New("ignore pattern is too long")

type ignoreMatcher struct {
	gitRules     *ruleMatcher
	ignoreRules  *ruleMatcher
	ripgrepRules *ruleMatcher
}

type ignoreCheckpoint struct {
	gitRules     int
	ignoreRules  int
	ripgrepRules int
}

func newIgnoreMatcher() *ignoreMatcher {
	return &ignoreMatcher{
		gitRules:     newRuleMatcher(),
		ignoreRules:  newRuleMatcher(),
		ripgrepRules: newRuleMatcher(),
	}
}

func (m *ignoreMatcher) checkpoint() ignoreCheckpoint {
	return ignoreCheckpoint{
		gitRules:     len(m.gitRules.sets),
		ignoreRules:  len(m.ignoreRules.sets),
		ripgrepRules: len(m.ripgrepRules.sets),
	}
}

func (m *ignoreMatcher) restore(checkpoint ignoreCheckpoint) {
	m.gitRules.restore(checkpoint.gitRules)
	m.ignoreRules.restore(checkpoint.ignoreRules)
	m.ripgrepRules.restore(checkpoint.ripgrepRules)
}

func (m *ignoreMatcher) ignored(path string, isDir bool) bool {
	if path == "" {
		return false
	}
	parts := strings.Split(path, "/")
	ignored := false
	for end := 1; end <= len(parts); end++ {
		ignored = m.childIgnoredParts(parts[:end], end < len(parts) || isDir, ignored)
	}
	return ignored
}

// childIgnored applies rules that directly match one path entry to the
// effective state inherited from its parent. A negation cannot reopen an
// entry below an ignored parent, but a caller may still traverse it because a
// direct query explicitly selected that subtree.
func (m *ignoreMatcher) childIgnored(path string, isDir, parentIgnored bool) bool {
	return m.childIgnoredParts(strings.Split(path, "/"), isDir, parentIgnored)
}

func (m *ignoreMatcher) childIgnoredParts(pathParts []string, isDir, parentIgnored bool) bool {
	matched, ignored := m.directDecisionParts(pathParts, isDir)
	return parentIgnored || (matched && ignored)
}

func (m *ignoreMatcher) directDecisionParts(pathParts []string, isDir bool) (bool, bool) {
	// Source precedence is evaluated for the current path level. A negation in
	// a higher-priority source wins at that level, but does not mask lower-source
	// rules that directly match descendants. The order matches ripgrep:
	// .rgignore, then .ignore, then Git's ignore sources.
	for _, matcher := range []*ruleMatcher{m.ripgrepRules, m.ignoreRules, m.gitRules} {
		matched, ignored := matcher.directDecisionParts(pathParts, isDir)
		if matched {
			return true, ignored
		}
	}
	return false, false
}

func (m *ignoreMatcher) addFile(matcher *ruleMatcher, abs, relDir string) error {
	info, err := os.Lstat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("filesearch: inspect ignore file %s: %w", abs, err)
	}
	// Ignore files encountered during traversal obey the same no-symlink
	// boundary as ordinary entries. Git also deliberately does not follow a
	// symlink in place of .gitignore.
	if !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(abs)
	if err != nil {
		return fmt.Errorf("filesearch: open ignore file %s: %w", abs, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("filesearch: inspect opened ignore file %s: %w", abs, err)
	}
	if !openedInfo.Mode().IsRegular() {
		return fmt.Errorf("filesearch: ignore file changed while opening: %s", abs)
	}
	// Verify the opened handle still refers to the regular file checked by
	// Lstat. This closes the substitution window without platform-specific
	// O_NOFOLLOW code; a replacement symlink resolves to a different file.
	if !os.SameFile(info, openedInfo) {
		return fmt.Errorf("filesearch: ignore file changed while opening: %s", abs)
	}
	if openedInfo.Size() > maxIgnoreFileBytes {
		return fmt.Errorf(
			"filesearch: ignore file %s exceeds %d bytes",
			abs, maxIgnoreFileBytes,
		)
	}
	patterns, err := readIgnorePatterns(file, abs)
	if err != nil {
		return err
	}
	if patternErrs := matcher.addPatternList(patterns, relDir, abs); len(patternErrs) > 0 {
		first := patternErrs[0]
		return fmt.Errorf(
			"filesearch: invalid ignore pattern in %s:%d: %s: %s",
			abs, first.line, first.pattern, first.message,
		)
	}
	return nil
}

func readIgnorePatterns(reader io.Reader, source string) ([]matcherPattern, error) {
	buffered := bufio.NewReaderSize(reader, initialIgnoreBufferBytes)
	var patterns []matcherPattern
	totalBytes := 0
	expandedBytes := 0
	for lineNumber := 1; ; lineNumber++ {
		raw, readErr := readIgnoreLine(buffered)
		if errors.Is(readErr, errIgnorePatternTooLong) {
			return nil, fmt.Errorf(
				"filesearch: ignore pattern in %s:%d exceeds %d bytes",
				source, lineNumber, maxPatternBytes,
			)
		}
		totalBytes += len(raw)
		if totalBytes > maxIgnoreFileBytes {
			return nil, fmt.Errorf(
				"filesearch: ignore file %s exceeds %d bytes",
				source, maxIgnoreFileBytes,
			)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, fmt.Errorf("filesearch: read ignore file %s: %w", source, readErr)
		}
		if len(raw) == 0 && errors.Is(readErr, io.EOF) {
			break
		}
		line := strings.TrimSuffix(string(raw), "\n")
		line = strings.TrimSuffix(line, "\r")
		parsed, ok := parseMatcherPatternLine(line, lineNumber)
		if !ok {
			if errors.Is(readErr, io.EOF) {
				break
			}
			continue
		}
		expanded, expandErr := expandBraces(parsed.text)
		if expandErr != nil {
			return nil, fmt.Errorf(
				"filesearch: invalid ignore pattern in %s:%d: %w",
				source, lineNumber, expandErr,
			)
		}
		if len(patterns)+len(expanded) > maxIgnorePatterns {
			return nil, fmt.Errorf(
				"filesearch: ignore file %s exceeds %d expanded patterns",
				source, maxIgnorePatterns,
			)
		}
		for _, candidate := range expanded {
			if len(candidate) > maxExpandedPatternBytes-expandedBytes {
				return nil, fmt.Errorf(
					"filesearch: ignore file %s exceeds %d expanded pattern bytes",
					source, maxExpandedPatternBytes,
				)
			}
			expandedBytes += len(candidate)
			patterns = append(patterns, matcherPattern{
				text:    candidate,
				display: parsed.display,
				negated: parsed.negated,
				line:    lineNumber,
			})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return patterns, nil
}

// readIgnoreLine uses a small buffer for ordinary files and allocates an
// accumulator only when a physical line crosses that buffer. The limit is on
// line contents: LF is framing and excluded, while a preceding CR is counted
// before CRLF normalization.
func readIgnoreLine(reader *bufio.Reader) ([]byte, error) {
	fragment, readErr := reader.ReadSlice('\n')
	if !errors.Is(readErr, bufio.ErrBufferFull) {
		if ignoreLineContentLength(fragment) > maxPatternBytes {
			return nil, errIgnorePatternTooLong
		}
		return fragment, readErr
	}

	line := append([]byte(nil), fragment...)
	if len(line) > maxPatternBytes {
		return nil, errIgnorePatternTooLong
	}
	for {
		fragment, readErr = reader.ReadSlice('\n')
		contentLength := len(line) + len(fragment)
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			contentLength--
		}
		if contentLength > maxPatternBytes {
			return nil, errIgnorePatternTooLong
		}
		line = append(line, fragment...)
		if !errors.Is(readErr, bufio.ErrBufferFull) {
			return line, readErr
		}
	}
}

func ignoreLineContentLength(raw []byte) int {
	length := len(raw)
	if length > 0 && raw[length-1] == '\n' {
		length--
	}
	return length
}

func (m *ignoreMatcher) loadRoot(root string, excluded func(string) bool) error {
	// In a linked worktree .git is a regular file whose target may live outside
	// the search root. Only read an exclude file reached through real, local
	// metadata directories.
	gitDir := filepath.Join(root, ".git")
	gitInfoDir := filepath.Join(gitDir, "info")
	gitDirOK, err := localDirectory(gitDir)
	if err != nil {
		return err
	}
	gitInfoDirOK := false
	if gitDirOK {
		gitInfoDirOK, err = localDirectory(gitInfoDir)
		if err != nil {
			return err
		}
	}
	if gitInfoDirOK {
		path := filepath.Join(gitInfoDir, "exclude")
		if excluded == nil || !excluded(path) {
			if addErr := m.addFile(m.gitRules, path, ""); addErr != nil {
				return addErr
			}
		}
	}
	return m.loadDirectory(root, "", excluded)
}

func localDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("filesearch: inspect metadata directory %s: %w", path, err)
	}
	return info.IsDir(), nil
}

func (m *ignoreMatcher) loadDirectory(abs, rel string, excluded func(string) bool) error {
	// Root and explicit-query ancestors are loaded before their directory
	// entries are available. Recursive traversal uses loadDirectoryEntries.
	for _, source := range []struct {
		name    string
		matcher *ruleMatcher
	}{
		{name: ".gitignore", matcher: m.gitRules},
		{name: ".ignore", matcher: m.ignoreRules},
		{name: ".rgignore", matcher: m.ripgrepRules},
	} {
		path := filepath.Join(abs, source.name)
		if excluded != nil && excluded(path) {
			continue
		}
		if err := m.addFile(source.matcher, path, rel); err != nil {
			return err
		}
	}
	return nil
}

func (m *ignoreMatcher) loadDirectoryEntries(abs, rel string, entries []os.DirEntry, excluded func(string) bool) error {
	// Reuse the listing that walkDirectory already needs. Trying all three
	// names with separate filesystem probes in every directory made missing
	// files a significant source of syscalls on deep trees.
	for _, entry := range entries {
		var matcher *ruleMatcher
		switch entry.Name() {
		case ".gitignore":
			matcher = m.gitRules
		case ".ignore":
			matcher = m.ignoreRules
		case ".rgignore":
			matcher = m.ripgrepRules
		default:
			continue
		}
		path := filepath.Join(abs, entry.Name())
		if excluded != nil && excluded(path) {
			continue
		}
		if err := m.addFile(matcher, path, rel); err != nil {
			return err
		}
	}
	return nil
}
