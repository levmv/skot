package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	jobProtocolVersion = 3
	// jobWorkerProtocolVersion versions the private stdin document exchanged
	// by a ProcessManager and its re-exec worker independently of durable state.
	jobWorkerProtocolVersion = 1

	jobMetadataFile  = "job.json"
	jobControlFile   = "control"
	jobResultFile    = "result.json"
	jobDeliveredFile = "delivered"
	jobStdoutFile    = "stdout.log"
	jobStderrFile    = "stderr.log"
	jobWorkerLogFile = "worker.log"
)

const (
	jobControlFD      = 3
	jobControlStop    = byte('s')
	jobControlMaxRead = 256
)

// jobMetadata is the durable descriptor used to adopt a job after its original
// ProcessManager disappears. It includes the display command, but not the
// execution environment or stdin; those belong to jobWorkerSpec and are never
// persisted by the job protocol.
type jobMetadata struct {
	Version        int       `json:"version"`
	JobID          string    `json:"job_id"`
	SessionID      string    `json:"session_id"`
	Command        string    `json:"command"`
	StartedAt      time.Time `json:"started_at"`
	TimeoutMillis  int64     `json:"timeout_ms"`
	SeparateStderr bool      `json:"separate_stderr,omitempty"`
	Scope          Scope     `json:"scope,omitempty"`
	Detach         bool      `json:"detach,omitempty"`
}

// jobWorkerSpec is the ephemeral half of a supervised launch. It is sent to a
// private re-exec over stdin and may contain secrets, so it must not be written
// into the durable job directory. Values shared with adoption come from the
// validated jobMetadata descriptor instead of being copied here.
type jobWorkerSpec struct {
	Version  int      `json:"version"`
	JobDir   string   `json:"job_dir"`
	LogLimit int64    `json:"log_limit"`
	Program  string   `json:"program"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Dir      string   `json:"dir"`
	Stdin    []byte   `json:"stdin,omitempty"`
}

type jobTerminalResult struct {
	Version          int       `json:"version"`
	JobID            string    `json:"job_id"`
	Started          bool      `json:"started"`
	Status           string    `json:"status"`
	ExitCode         *int      `json:"exit_code,omitempty"`
	Error            string    `json:"error,omitempty"`
	OutputError      string    `json:"output_error,omitempty"`
	StopReason       string    `json:"stop_reason,omitempty"`
	ManagedProcesses int       `json:"managed_processes,omitempty"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	StdoutBytes      int64     `json:"stdout_bytes"`
	StdoutDiscarded  int64     `json:"stdout_discarded,omitempty"`
	StderrBytes      int64     `json:"stderr_bytes,omitempty"`
	StderrDiscarded  int64     `json:"stderr_discarded,omitempty"`
}

func sessionJobHome(jobHome, sessionID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(sessionID)))
	return filepath.Join(jobHome, hex.EncodeToString(digest[:16]))
}

func jobDirectory(jobHome, sessionID, jobID string) string {
	return filepath.Join(sessionJobHome(jobHome, sessionID), jobID)
}

func jobControlPath(jobDir string) string {
	return filepath.Join(jobDir, jobControlFile)
}

func writeJSONAtomic(path string, value any, mode os.FileMode) (returnErr error) {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, temporary.Close())
		}
		if returnErr != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}

func readJSONFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func loadJobMetadata(jobDir string) (jobMetadata, error) {
	var metadata jobMetadata
	if err := readJSONFile(filepath.Join(jobDir, jobMetadataFile), &metadata); err != nil {
		return jobMetadata{}, err
	}
	if err := validateJobMetadata(metadata, jobDir); err != nil {
		return jobMetadata{}, err
	}
	return metadata, nil
}

func validateJobMetadata(metadata jobMetadata, jobDir string) error {
	if metadata.Version != jobProtocolVersion {
		return fmt.Errorf("unsupported job protocol version %d", metadata.Version)
	}
	if metadata.JobID == "" || filepath.Base(jobDir) != metadata.JobID {
		return errors.New("job id does not match its directory")
	}
	if strings.TrimSpace(metadata.Command) == "" || metadata.StartedAt.IsZero() || metadata.TimeoutMillis <= 0 {
		return errors.New("job timing metadata is invalid")
	}
	if err := validateConcreteScope(metadata.Scope); err != nil {
		return fmt.Errorf("job filesystem scope is invalid: %w", err)
	}
	return nil
}

func validateJobSessionOwnership(metadata jobMetadata, sessionID string) error {
	if metadata.SessionID != strings.TrimSpace(sessionID) {
		return fmt.Errorf("job belongs to session %q", metadata.SessionID)
	}
	return nil
}

func readJobTerminalResult(jobDir, jobID string) (jobTerminalResult, bool, error) {
	var result jobTerminalResult
	err := readJSONFile(filepath.Join(jobDir, jobResultFile), &result)
	if errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if result.Version != jobProtocolVersion || result.JobID != jobID || !validTerminalProcessStatus(result.Status) || result.FinishedAt.IsZero() {
		return result, false, errors.New("terminal job result is invalid")
	}
	return result, true, nil
}

func validTerminalProcessStatus(status string) bool {
	switch status {
	case ProcessCompleted, ProcessFailed, ProcessKilled, ProcessTimedOut, ProcessNotStarted:
		return true
	default:
		return false
	}
}

func jobDelivered(jobDir string) (bool, error) {
	_, err := os.Stat(filepath.Join(jobDir, jobDeliveredFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

func markJobDelivered(jobDir string) error {
	path := filepath.Join(jobDir, jobDeliveredFile)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return file.Close()
}
