package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/levmv/skot/agent"
	productlimits "github.com/levmv/skot/internal/limits"
)

const journalName = "events.jsonl"

type Summary struct {
	ID        string
	Title     string
	UpdatedAt time.Time
}

func ResolveHome(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home: %w", err)
		}
		value = filepath.Join(home, ".skot")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve Skot home: %w", err)
	}
	return filepath.Clean(abs), nil
}

func Create(home string) (*Store, string, error) {
	home, err := ResolveHome(home)
	if err != nil {
		return nil, "", err
	}
	sessionsDir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("create sessions directory: %w", err)
	}
	if err := os.Chmod(sessionsDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("restrict sessions directory: %w", err)
	}
	for attempt := 0; attempt < 8; attempt++ {
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
	home, err := ResolveHome(home)
	if err != nil {
		return nil, err
	}
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
	return Open(path)
}

func List(home, workspace string) ([]Summary, error) {
	home, err := ResolveHome(home)
	if err != nil {
		return nil, err
	}
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
	if absolute, err := filepath.Abs(workspace); err == nil {
		workspace = absolute
	}
	workspace = filepath.Clean(workspace)
	current := workspace
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return workspace
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func sessionTitle(value string) string {
	const maxRunes = 72
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	for _, line := range strings.Split(value, "\n") {
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
