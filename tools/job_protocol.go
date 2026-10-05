package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
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
	SeparateStderr bool      `json:"separate_stderr,omitzero"`
	Scope          Scope     `json:"scope,omitempty"`
	Detach         bool      `json:"detach,omitzero"`
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
	ExitCode         *int      `json:"exit_code,omitzero"`
	Error            string    `json:"error,omitempty"`
	OutputError      string    `json:"output_error,omitempty"`
	StopReason       string    `json:"stop_reason,omitempty"`
	ManagedProcesses int       `json:"managed_processes,omitzero"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	StdoutBytes      int64     `json:"stdout_bytes"`
	StdoutDiscarded  int64     `json:"stdout_discarded,omitzero"`
	StderrBytes      int64     `json:"stderr_bytes,omitzero"`
	StderrDiscarded  int64     `json:"stderr_discarded,omitzero"`
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
	directory, err := openJobDirectory(filepath.Dir(path), false)
	if err != nil {
		return err
	}
	defer directory.Close()
	temporary, err := createJobTemp(directory, filepath.Base(path))
	if err != nil {
		return err
	}
	temporaryPath := filepath.Base(temporary.Name())
	closed := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, temporary.Close())
		}
		if returnErr != nil {
			_ = directory.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if _, err := temporary.Write(encoded); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	if err := directory.Rename(temporaryPath, filepath.Base(path)); err != nil {
		return err
	}
	return nil
}

func readJSONFile(path string, target any) error {
	file, err := openJobFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular job file", path)
	}
	// Durable metadata cannot reasonably exceed the complete worker launch.
	data, err := io.ReadAll(io.LimitReader(file, maxJobWorkerSpecBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxJobWorkerSpecBytes {
		return fmt.Errorf("job file is too large: %s", path)
	}
	return json.Unmarshal(data, target, json.RejectUnknownMembers(true))
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
	if err := validateScope(metadata.Scope); err != nil {
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
	directory, err := openJobDirectory(jobDir, false)
	if err != nil {
		return false, err
	}
	defer directory.Close()
	_, err = directory.Stat(jobDeliveredFile)
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
	file, err := openJobFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return file.Close()
}
