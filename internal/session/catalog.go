package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	Title     string
	UpdatedAt time.Time
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
	if !validSessionID(id) {
		return nil, fmt.Errorf("invalid session ID %q", id)
	}
	path := filepath.Join(home, "sessions", id, journalName)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("session %q does not exist", id)
		}
		return nil, fmt.Errorf("stat session %q: %w", id, err)
	}
	if err := privatefs.RequireRegularFile(path, "managed session journal"); err != nil {
		return nil, err
	}
	privatefs.TryRestrictPermissions(path)
	return Open(path)
}

func List(home, workspace string) ([]Summary, error) {
	workspace = canonicalWorkspace(workspace)
	entries, err := os.ReadDir(filepath.Join(home, "sessions"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read sessions: %w", err)
	}
	var summaries []Summary
	for _, entry := range entries {
		if !entry.IsDir() || !validSessionID(entry.Name()) {
			continue
		}
		path := filepath.Join(home, "sessions", entry.Name(), journalName)
		if summary, ok := summarize(entry.Name(), path, workspace); ok {
			summaries = append(summaries, summary)
		}
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
	summaries, err := List(home, workspace)
	if err != nil {
		return Summary{}, err
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		if len(summaries) == 0 {
			return Summary{}, fmt.Errorf("no resumable sessions in workspace %s", workspace)
		}
		return summaries[0], nil
	}
	if !strings.HasPrefix(prefix, "session_") {
		prefix = "session_" + prefix
	}
	var matches []Summary
	for _, summary := range summaries {
		if strings.HasPrefix(summary.ID, prefix) {
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

func summarize(id, path, workspace string) (Summary, bool) {
	file, err := os.Open(path)
	if err != nil {
		return Summary{}, false
	}
	defer file.Close()
	summary := Summary{ID: id}
	if info, err := file.Stat(); err == nil {
		summary.UpdatedAt = info.ModTime()
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, initialJournalBufferBytes), productlimits.MaxJournalRecordBytes+1)
	if !scanner.Scan() {
		return Summary{}, false
	}
	var record agent.Record
	if json.Unmarshal(scanner.Bytes(), &record) != nil || record.Kind != agent.RecordSessionStarted {
		return Summary{}, false
	}
	var started agent.SessionStartedRecord
	if json.Unmarshal(record.Data, &started) != nil || started.SchemaVersion != agent.JournalSchemaVersion ||
		started.SessionID != id || canonicalWorkspace(started.Workspace) != workspace {
		return Summary{}, false
	}
	for scanner.Scan() {
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			return Summary{}, false
		}
		if record.Kind != agent.RecordRunInputAdded {
			continue
		}
		var input agent.RunInputAddedRecord
		if json.Unmarshal(record.Data, &input) != nil {
			return Summary{}, false
		}
		summary.Title = sessionTitle(input.Text)
		if summary.Title != "" {
			return summary, true
		}
	}
	return Summary{}, false
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

func validSessionID(id string) bool {
	if len(id) != len("session_")+32 || !strings.HasPrefix(id, "session_") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(id, "session_"))
	return err == nil
}
