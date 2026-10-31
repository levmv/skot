package session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/canonicalpath"
	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/privatefs"
)

const journalName = "events.jsonl"

type Summary struct {
	ID        string
	Workspace string
	Title     string
	UpdatedAt time.Time
	Locked    bool
	Error     string
}

func Create(home string) (*Store, string, error) {
	sessionsDir := filepath.Join(home, "sessions")
	if err := privatefs.EnsureDirectory(sessionsDir, "sessions directory"); err != nil {
		return nil, "", err
	}
	privatefs.TryRestrictPermissions(sessionsDir)
	for range 8 {
		id, err := newSessionID()
		if err != nil {
			return nil, "", err
		}
		dir := filepath.Join(sessionsDir, id)
		if err := os.Mkdir(dir, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return nil, "", fmt.Errorf("create session directory: %w", err)
		}
		store, err := Open(filepath.Join(dir, journalName))
		if err != nil {
			_ = os.Remove(dir)
			return nil, "", err
		}
		return store, id, nil
	}
	return nil, "", errors.New("could not allocate a unique session ID")
}

func OpenManaged(home, id string) (*Store, error) {
	file, err := openManagedJournal(home, id, os.O_RDWR|os.O_APPEND)
	if err != nil {
		return nil, err
	}
	privatefs.TryRestrictOpenFile(file)
	return openStore(file, file.Name())
}

func List(home, workspace string) ([]Summary, error) {
	workspace = canonicalWorkspace(workspace)
	return listSummaries(context.Background(), home, &workspace)
}

func listSummaries(ctx context.Context, home string, workspace *string) ([]Summary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(home, "sessions")
	if err := privatefs.InspectDirectory(dir, "sessions directory"); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sessions: %w", err)
	}
	var summaries []Summary
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !IsFullID(entry.Name()) {
			continue
		}
		summary, err := summarize(ctx, home, entry.Name())
		if err != nil {
			// A concurrently removed session is no longer a catalog entry.
			if errors.Is(err, os.ErrNotExist) {
				if _, statErr := os.Lstat(filepath.Join(dir, entry.Name())); errors.Is(statErr, os.ErrNotExist) {
					continue
				}
			}
			summary.Error = err.Error()
		} else if summary.Title == "" {
			continue
		}
		if workspace != nil && summary.Workspace != *workspace {
			continue
		}
		summaries = append(summaries, summary)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		if !summaries[i].UpdatedAt.Equal(summaries[j].UpdatedAt) {
			return summaries[i].UpdatedAt.After(summaries[j].UpdatedAt)
		}
		return summaries[i].ID < summaries[j].ID
	})
	return summaries, nil
}

func Resolve(home, workspace, prefix string) (Summary, error) {
	workspace = canonicalWorkspace(workspace)
	prefix = strings.TrimSpace(prefix)
	if prefix != "" && !strings.HasPrefix(prefix, "session_") {
		prefix = "session_" + prefix
	}
	// A full ID is globally unique. An empty workspace is allowed only for
	// this direct lookup, so Open can resolve its root before building tools.
	if IsFullID(prefix) {
		summary, err := summarize(context.Background(), home, prefix)
		if err != nil {
			return Summary{}, err
		}
		if summary.Title == "" || summary.Workspace == "" || (workspace != "" && summary.Workspace != workspace) {
			return Summary{}, fmt.Errorf("no session matching %q in workspace %s", prefix, workspace)
		}
		return summary, nil
	}
	summaries, err := List(home, workspace)
	if err != nil {
		return Summary{}, err
	}
	if prefix == "" {
		for _, summary := range summaries {
			if summary.Error == "" {
				return summary, nil
			}
		}
		return Summary{}, fmt.Errorf("no resumable sessions in workspace %s", workspace)
	}
	var matches []Summary
	for _, summary := range summaries {
		if summary.Error == "" && strings.HasPrefix(summary.ID, prefix) {
			matches = append(matches, summary)
		}
	}
	if len(matches) == 0 {
		return Summary{}, fmt.Errorf("no session matching %q in workspace %s", prefix, workspace)
	}
	if len(matches) > 1 {
		return Summary{}, fmt.Errorf("session prefix %q is ambiguous", prefix)
	}
	return matches[0], nil
}

// summarize stops after the first nonempty input. Catalogs report metadata
// failures; validating the rest of a journal belongs to Inspect or OpenManaged.
func summarize(ctx context.Context, home, id string) (Summary, error) {
	summary := Summary{ID: id}
	file, err := openManagedJournal(home, id, os.O_RDONLY)
	if err != nil {
		return summary, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return summary, err
	}
	summary.UpdatedAt = info.ModTime()
	summary.Locked, err = journalLocked(file)
	if err != nil {
		return summary, err
	}
	scanner := bufio.NewScanner(io.NewSectionReader(file, 0, info.Size()))
	scanner.Buffer(make([]byte, initialJournalBufferBytes), productlimits.MaxJournalRecordBytes+1)
	scanner.Split(completeJournalLines)
	var sequence uint64
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		sequence++
		var record agent.Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return summary, fmt.Errorf("decode journal record at line %d: %w", sequence, err)
		}
		if record.Sequence != sequence || record.Time.IsZero() || record.Kind == "" || len(record.Data) == 0 {
			return summary, fmt.Errorf("invalid journal record at line %d", sequence)
		}
		if sequence == 1 {
			if record.Kind != agent.RecordSessionStarted {
				return summary, errors.New("first journal record must be session_started")
			}
			var started agent.SessionStartedRecord
			if err := json.Unmarshal(record.Data, &started); err != nil {
				return summary, err
			}
			if started.SessionID != id {
				return summary, errors.New("journal session ID does not match its directory")
			}
			summary.Workspace = canonicalWorkspace(started.Workspace)
			if started.SchemaVersion != agent.JournalSchemaVersion {
				return summary, fmt.Errorf("unsupported journal schema version %d", started.SchemaVersion)
			}
			continue
		}
		if record.Kind != agent.RecordRunInputAdded {
			continue
		}
		var input agent.RunInputAddedRecord
		if err := json.Unmarshal(record.Data, &input); err != nil {
			return summary, err
		}
		summary.Title = sessionTitle(input.Text)
		if summary.Title != "" {
			return summary, nil
		}
	}
	return summary, scanner.Err()
}

// A live writer owns the final unterminated line, even if it is valid JSON.
func completeJournalLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if index := bytes.IndexByte(data, '\n'); index >= 0 {
		return index + 1, data[:index], nil
	}
	if atEOF {
		return len(data), nil, nil
	}
	return 0, nil, nil
}

func canonicalWorkspace(workspace string) string {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return ""
	}
	return canonicalpath.Resolve(workspace)
}

func sessionTitle(value string) string {
	const maxRunes = 72
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	for line := range strings.SplitSeq(value, "\n") {
		line = strings.Join(strings.FieldsFunc(line, unicode.IsSpace), " ")
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > maxRunes {
			return string(runes[:maxRunes-1]) + "…"
		}
		return line
	}
	return ""
}

func ShortID(id string) string {
	value := strings.TrimPrefix(id, "session_")
	if len(value) > 12 {
		value = value[:12]
	}
	return value
}

// LooksLikeIDPrefix reports whether prefix could address a session. IDs are
// hex, so a prefix holding any other character can never match one, which in
// practice means it is the second word of a prompt starting with a command
// name rather than a session the caller expected to find.
func LooksLikeIDPrefix(prefix string) bool {
	prefix = strings.TrimPrefix(strings.TrimSpace(prefix), "session_")
	for _, char := range prefix {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func newSessionID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	return "session_" + hex.EncodeToString(data[:]), nil
}

// IsFullID reports whether id is a complete managed session identifier.
func IsFullID(id string) bool {
	if len(id) != len("session_")+32 || !strings.HasPrefix(id, "session_") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "session_"))
	return err == nil
}
