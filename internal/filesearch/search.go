package filesearch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"unicode/utf8"
)

const (
	textCheckBufferBytes = 64 * 1024
	maxSearchLineBytes   = 64 * 1024 * 1024
)

type searchOperation struct {
	regexp           *regexp.Regexp
	literal          []byte
	foldLiteral      *foldLiteralMatcher
	requiredLiterals [][]byte
	previewBytes     int
	maxLineBytes     int
	visit            func(Match) error
	stats            Stats
	checkBuffer      []byte
	reader           *bufio.Reader
	exclude          func(string) bool
}

type searchBuffers struct {
	checkBuffer []byte
	reader      *bufio.Reader
}

type exhaustedReader struct{}

func (exhaustedReader) Read([]byte) (int, error) { return 0, io.EOF }

// A search needs two reusable 64 KiB buffers. Pooling them avoids that fixed
// allocation on every agent query. Resetting the reader before Put detaches
// the last *os.File so the pool cannot retain it.
var searchBufferPool = sync.Pool{New: func() any {
	return &searchBuffers{
		checkBuffer: make([]byte, textCheckBufferBytes+utf8.UTFMax),
		reader:      bufio.NewReaderSize(exhaustedReader{}, textCheckBufferBytes),
	}
}}

// Search visits matching lines in deterministic path and line order. Files
// containing NUL or invalid UTF-8 anywhere are skipped as a whole. Lines are
// matched in full before an optional preview is constructed.
func (s *Searcher) Search(ctx context.Context, query SearchQuery, visit func(Match) error) (Stats, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if visit == nil {
		return Stats{}, errors.New("filesearch: Search callback is nil")
	}
	if query.Pattern == "" {
		return Stats{}, errors.New("filesearch: search pattern is required")
	}
	if len(query.Pattern) > maxPatternBytes {
		return Stats{}, fmt.Errorf("filesearch: search pattern exceeds %d bytes", maxPatternBytes)
	}
	if query.PreviewBytes < 0 {
		return Stats{}, errors.New("filesearch: preview byte limit must not be negative")
	}
	compiled, err := regexp.Compile(query.Pattern)
	if err != nil {
		return Stats{}, fmt.Errorf("filesearch: compile regexp %q: %w", query.Pattern, err)
	}
	var direct *directPattern
	if query.Include != "" {
		direct, err = compileDirectPattern(query.Include)
		if err != nil {
			return Stats{}, err
		}
	}
	resolved, err := s.resolveExistingPath(query.Path)
	if err != nil {
		return Stats{}, err
	}
	operation := searchOperation{
		regexp:       compiled,
		previewBytes: query.PreviewBytes,
		maxLineBytes: maxSearchLineBytes,
		visit:        visit,
		exclude:      s.exclude,
	}
	buffers := searchBufferPool.Get().(*searchBuffers)
	operation.checkBuffer = buffers.checkBuffer
	operation.reader = buffers.reader
	defer func() {
		buffers.reader.Reset(exhaustedReader{})
		searchBufferPool.Put(buffers)
	}()
	// These fast paths preserve regexp.Match semantics. A required-literal set
	// only rejects impossible lines; the regexp still confirms every candidate.
	if utf8.ValidString(query.Pattern) && regexp.QuoteMeta(query.Pattern) == query.Pattern {
		operation.literal = []byte(query.Pattern)
	} else if foldLiteral := regexpFoldLiteral(query.Pattern); foldLiteral != nil {
		operation.foldLiteral = foldLiteral
	} else {
		operation.requiredLiterals = regexpRequiredLiterals(query.Pattern)
	}
	if !resolved.info.IsDir() {
		return operation.searchExplicitFile(ctx, resolved, direct)
	}

	walker := fileWalker{
		searcher: s,
		query:    resolved.directory(),
		direct:   direct,
		visit: func(file walkedFile) error {
			skipped, scanErr := operation.scanFile(ctx, file.abs, file.path)
			if skipped {
				operation.stats.FilesSkipped++
			}
			return scanErr
		},
	}
	walkStats, walkErr := walker.run(ctx)
	operation.stats.FilesVisited = walkStats.FilesVisited
	operation.stats.FilesSkipped += walkStats.FilesSkipped
	operation.stats.Stopped = walkStats.Stopped
	return operation.stats, walkErr
}

func (o *searchOperation) searchExplicitFile(
	ctx context.Context,
	path resolvedPath,
	direct *directPattern,
) (Stats, error) {
	if pathInsideGitMetadata(path.rel) || pathInsideGitMetadata(path.resolvedRel) {
		return o.stats, nil
	}
	o.stats.FilesVisited = 1
	if direct != nil {
		matched := direct.matches(path.rel, false)
		if direct.include != matched {
			o.stats.FilesSkipped = 1
			return o.stats, nil
		}
	}
	skipped, err := o.scanFile(ctx, path.resolved, path.rel)
	if skipped {
		o.stats.FilesSkipped = 1
	}
	if errors.Is(err, ErrStop) {
		o.stats.Stopped = true
		return o.stats, nil
	}
	return o.stats, err
}

func (o *searchOperation) scanFile(ctx context.Context, abs, display string) (bool, error) {
	// Recheck immediately before open so a policy enabled during traversal
	// cannot leave later files readable.
	if o.exclude != nil && o.exclude(abs) {
		return true, nil
	}
	file, err := os.Open(abs)
	if err != nil {
		return false, fmt.Errorf("filesearch: open %s: %w", display, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("filesearch: stat %s: %w", display, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("filesearch: path changed and is no longer a regular file: %s", display)
	}
	// Validate the whole file before emitting a match. Otherwise a NUL or
	// invalid UTF-8 sequence near EOF could leave callers with partial results.
	validText, err := validateTextFile(ctx, file, o.checkBuffer)
	if err != nil {
		return false, fmt.Errorf("filesearch: validate %s: %w", display, err)
	}
	if !validText {
		return true, nil
	}
	if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
		return false, fmt.Errorf("filesearch: rewind %s: %w", display, seekErr)
	}
	o.reader.Reset(file)
	for lineNumber := 1; ; lineNumber++ {
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		raw, readErr := readBoundedLine(ctx, o.reader, o.maxLineBytes)
		if errors.Is(readErr, errLineTooLong) {
			o.stats.OversizedLinesSkipped++
			if errors.Is(readErr, io.EOF) {
				return false, nil
			}
			continue
		}
		if len(raw) == 0 && errors.Is(readErr, io.EOF) {
			return false, nil
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false, fmt.Errorf("filesearch: read %s: %w", display, readErr)
		}

		line := trimLineEnding(raw)
		// The file may have changed after validation and rewind. Recheck the
		// bytes being matched rather than silently violating the text contract.
		if bytes.IndexByte(line, 0) >= 0 || !utf8.Valid(line) {
			return false, fmt.Errorf("filesearch: %s changed during search", display)
		}
		var matched bool
		switch {
		case o.literal != nil:
			matched = bytes.Contains(line, o.literal)
		case o.foldLiteral != nil:
			matched = o.foldLiteral.matches(line)
		case len(o.requiredLiterals) == 0 || containsRequiredLiteral(line, o.requiredLiterals):
			matched = o.regexp.Match(line)
		}
		if matched {
			text := string(line)
			preview, truncated := previewLine(text, o.previewBytes)
			if visitErr := o.visit(Match{
				Path:          display,
				Line:          lineNumber,
				Text:          preview,
				LineTruncated: truncated,
			}); visitErr != nil {
				return false, visitErr
			}
			o.stats.Results++
		}
		if errors.Is(readErr, io.EOF) {
			return false, nil
		}
	}
}

func validateTextFile(ctx context.Context, file *os.File, buffer []byte) (bool, error) {
	if len(buffer) < textCheckBufferBytes+utf8.UTFMax {
		return false, errors.New("text validation buffer is too small")
	}
	carry := 0
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, err := file.Read(buffer[carry : carry+textCheckBufferBytes])
		total := carry + n
		if bytes.IndexByte(buffer[:total], 0) >= 0 {
			return false, nil
		}
		validEnd := completeUTF8Prefix(buffer[:total])
		if !utf8.Valid(buffer[:validEnd]) {
			return false, nil
		}
		carry = copy(buffer, buffer[validEnd:total])
		if errors.Is(err, io.EOF) {
			return carry == 0, nil
		}
		if err != nil {
			return false, err
		}
		if n == 0 {
			return false, io.ErrNoProgress
		}
	}
}

func completeUTF8Prefix(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	start := len(data) - 1
	for start >= 0 && len(data)-start <= utf8.UTFMax && !utf8.RuneStart(data[start]) {
		start--
	}
	if start >= 0 && !utf8.FullRune(data[start:]) {
		return start
	}
	return len(data)
}

var errLineTooLong = errors.New("line exceeds search limit")

// readBoundedLine consumes one complete line even when it exceeds limit. This
// lets callers skip a generated blob without losing synchronization or
// aborting searches in every other file.
func readBoundedLine(ctx context.Context, reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		fragment, readErr := reader.ReadSlice('\n')
		if !tooLong && len(line)+len(fragment) > limit+2 { // permit CRLF outside the content limit
			line = nil
			tooLong = true
		}
		if !tooLong && len(line) == 0 && !errors.Is(readErr, bufio.ErrBufferFull) {
			if len(trimLineEnding(fragment)) > limit {
				tooLong = true
			} else {
				return fragment, readErr
			}
		}
		if !tooLong {
			line = append(line, fragment...)
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, readErr
		}
		if !tooLong && len(trimLineEnding(line)) > limit {
			tooLong = true
		}
		if tooLong {
			if errors.Is(readErr, io.EOF) {
				return nil, errors.Join(errLineTooLong, io.EOF)
			}
			return nil, errLineTooLong
		}
		return line, readErr
	}
}

func trimLineEnding(line []byte) []byte {
	if len(line) == 0 || line[len(line)-1] != '\n' {
		return line
	}
	line = line[:len(line)-1]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}
