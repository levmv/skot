package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	productlimits "github.com/levmv/skot/internal/limits"
)

const (
	jobWorkerArg = "__sk_job_worker"
	// The launch document JSON-encodes tool input as base64. Two completion
	// ceilings leave room for that expansion plus argv and environment without
	// introducing a smaller accidental limit than the model response itself.
	maxJobWorkerSpecBytes = 2 * productlimits.MaxModelCompletionBytes

	// Keep the model-visible tail small while amortizing atomic compaction over
	// a larger transient file. Tests with deliberately tiny tails retain the
	// same ratio without having to write multiple MiB per fault injection.
	maxDurableTailCompactionThreshold = 8 * 1024 * 1024
	durableTailCompactionMultiple     = 32
)

// RunJobWorkerIfRequested handles Skot's private re-exec mode. Applications
// embedding ProcessManager must call it before normal argument parsing, just as
// the Skot binary does for the filesystem-boundary child mode.
func RunJobWorkerIfRequested() bool {
	if len(os.Args) < 2 || os.Args[1] != jobWorkerArg {
		return false
	}
	hardenSupervisor()
	if err := runJobWorker(os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "job worker: %v\n", err)
		os.Exit(125)
	}
	return true
}

func runJobWorker(input io.Reader) error {
	limited := io.LimitReader(input, maxJobWorkerSpecBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read launch specification: %w", err)
	}
	if len(data) > maxJobWorkerSpecBytes {
		return errors.New("launch specification is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var spec jobWorkerSpec
	if err := decoder.Decode(&spec); err != nil {
		return fmt.Errorf("decode launch specification: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("launch specification contains multiple JSON values")
	}
	if err := validateWorkerSpec(spec); err != nil {
		return err
	}

	var metadata jobMetadata
	if err := readJSONFile(filepath.Join(spec.JobDir, jobMetadataFile), &metadata); err != nil {
		return fmt.Errorf("read job metadata: %w", err)
	}
	if err := validateJobMetadata(metadata, spec.JobDir, metadata.SessionID); err != nil || metadata.JobID != spec.JobID {
		if err == nil {
			err = errors.New("launch job id does not match metadata")
		}
		return err
	}
	control, dummyWriter, err := inheritJobControl(jobControlPath(spec.JobDir))
	if err != nil {
		return err
	}
	defer func() {
		_ = dummyWriter.Close()
		_ = control.Close()
	}()
	stopRequests := make(chan struct{}, 1)
	go func() { _ = readJobControl(control, stopRequests) }()

	stdout, err := newDurableTailWriter(filepath.Join(spec.JobDir, jobStdoutFile), spec.LogLimit)
	if err != nil {
		return finishUnstartedWorker(spec, fmt.Errorf("open stdout log: %w", err))
	}
	var stderr *durableTailWriter
	if spec.SeparateStderr {
		stderr, err = newDurableTailWriter(filepath.Join(spec.JobDir, jobStderrFile), spec.LogLimit)
		if err != nil {
			_ = stdout.Close()
			return finishUnstartedWorker(spec, fmt.Errorf("open stderr log: %w", err))
		}
	}

	command := &exec.Cmd{
		Path:   spec.Program,
		Args:   append([]string(nil), spec.Args...),
		Env:    append([]string(nil), spec.Env...),
		Dir:    spec.Dir,
		Stdin:  bytes.NewReader(spec.Stdin),
		Stdout: stdout,
		Stderr: stdout,
	}
	if stderr != nil {
		command.Stderr = stderr
	}
	configureProcessGroup(command)
	startedAt := time.Now().UTC()
	if err := command.Start(); err != nil {
		_ = stdout.Close()
		if stderr != nil {
			_ = stderr.Close()
		}
		return finishUnstartedWorker(spec, fmt.Errorf("start process: %w", err))
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	timer := time.NewTimer(time.Duration(spec.TimeoutMillis) * time.Millisecond)
	var waitErr error
	status := ProcessUnknown
	stopReason := ""
	managedProcesses := 0
	select {
	case waitErr = <-wait:
		timer.Stop()
	case <-timer.C:
		status = ProcessTimedOut
		stopReason = "timeout after " + (time.Duration(spec.TimeoutMillis) * time.Millisecond).String()
		managedProcesses, waitErr = waitWhileKillingProcessGroup(command, wait)
	case <-stopRequests:
		timer.Stop()
		status = ProcessKilled
		stopReason = "stop requested"
		managedProcesses, waitErr = waitWhileKillingProcessGroup(command, wait)
	}
	if status == ProcessUnknown {
		if waitErr == nil {
			status = ProcessCompleted
		} else {
			status = ProcessFailed
		}
	}

	stdoutCloseErr := stdout.Close()
	var stderrCloseErr error
	if stderr != nil {
		stderrCloseErr = stderr.Close()
	}
	var outputErrs []string
	if stdoutCloseErr != nil {
		outputErrs = append(outputErrs, "stdout log: "+stdoutCloseErr.Error())
	}
	if stderrCloseErr != nil {
		outputErrs = append(outputErrs, "stderr log: "+stderrCloseErr.Error())
	}
	result := jobTerminalResult{
		Version:          jobProtocolVersion,
		JobID:            spec.JobID,
		Started:          true,
		Status:           status,
		ExitCode:         processExitCode(waitErr),
		Error:            processErrorText(waitErr),
		OutputError:      strings.Join(outputErrs, "; "),
		StopReason:       strings.TrimSpace(stopReason),
		ManagedProcesses: managedProcesses,
		StartedAt:        startedAt,
		FinishedAt:       time.Now().UTC(),
		StdoutBytes:      stdout.stored,
		StdoutDiscarded:  stdout.discarded,
	}
	if stderr != nil {
		result.StderrBytes = stderr.stored
		result.StderrDiscarded = stderr.discarded
	}
	if result.Status == ProcessCompleted {
		zero := 0
		result.ExitCode = &zero
	}
	if err := writeJSONAtomic(filepath.Join(spec.JobDir, jobResultFile), result, 0o600); err != nil {
		return fmt.Errorf("write terminal result: %w", err)
	}
	return nil
}

func validateWorkerSpec(spec jobWorkerSpec) error {
	if spec.Version != jobProtocolVersion || spec.JobID == "" {
		return errors.New("invalid worker protocol metadata")
	}
	if !filepath.IsAbs(spec.JobDir) || filepath.Base(filepath.Clean(spec.JobDir)) != spec.JobID {
		return errors.New("invalid worker job directory")
	}
	if spec.TimeoutMillis <= 0 || spec.LogLimit <= 0 {
		return errors.New("invalid worker limits")
	}
	if spec.Program == "" || len(spec.Args) == 0 || spec.Args[0] == "" || spec.Dir == "" {
		return errors.New("invalid worker command")
	}
	return nil
}

func finishUnstartedWorker(spec jobWorkerSpec, cause error) error {
	result := jobTerminalResult{
		Version:    jobProtocolVersion,
		JobID:      spec.JobID,
		Status:     ProcessNotStarted,
		Error:      cause.Error(),
		FinishedAt: time.Now().UTC(),
	}
	if err := writeJSONAtomic(filepath.Join(spec.JobDir, jobResultFile), result, 0o600); err != nil {
		return errors.Join(cause, fmt.Errorf("write launch failure: %w", err))
	}
	return cause
}

type durableTailWriter struct {
	file      *os.File
	path      string
	limit     int64
	compactAt int64
	size      int64
	received  int64
	stored    int64
	discarded int64
	closed    bool
	writeErr  error
}

func newDurableTailWriter(path string, limit int64) (*durableTailWriter, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &durableTailWriter{
		file:      file,
		path:      path,
		limit:     limit,
		compactAt: durableTailCompactionThreshold(limit),
	}, nil
}

func durableTailCompactionThreshold(limit int64) int64 {
	if limit <= 0 {
		return 0
	}
	minimum := int64(math.MaxInt64)
	if limit <= math.MaxInt64/2 {
		minimum = 2 * limit
	}
	threshold := int64(maxDurableTailCompactionThreshold)
	if limit <= math.MaxInt64/durableTailCompactionMultiple {
		threshold = min(threshold, durableTailCompactionMultiple*limit)
	}
	return max(minimum, threshold)
}

func (writer *durableTailWriter) Write(data []byte) (int, error) {
	original := len(data)
	writer.received += int64(original)
	if writer.limit <= 0 {
		writer.discarded = writer.received
		return original, nil
	}
	if writer.writeErr != nil {
		writer.updateDiscarded()
		return original, nil
	}
	if int64(len(data)) >= writer.compactAt {
		data = data[len(data)-int(writer.limit):]
		if err := writer.replace(data); err != nil {
			writer.noteWriteError(fmt.Errorf("replace durable output: %w", err))
		}
		writer.updateDiscarded()
		return original, nil
	}
	written, err := writer.file.Write(data)
	writer.size += int64(written)
	if err != nil {
		writer.noteWriteError(fmt.Errorf("write durable output: %w", err))
	} else if written != original {
		writer.noteWriteError(fmt.Errorf("write durable output: %w", io.ErrShortWrite))
	}
	if writer.writeErr == nil && writer.size > writer.compactAt {
		if err := writer.compact(); err != nil {
			writer.noteWriteError(fmt.Errorf("compact durable output: %w", err))
		}
	}
	writer.updateDiscarded()
	return original, nil
}

func (writer *durableTailWriter) noteWriteError(err error) {
	if err != nil && writer.writeErr == nil {
		writer.writeErr = err
	}
}

func (writer *durableTailWriter) updateDiscarded() {
	writer.discarded = writer.received - min(writer.size, writer.limit)
}

func (writer *durableTailWriter) compact() error {
	if writer.size <= writer.limit {
		return nil
	}
	keep := min(writer.size, writer.limit)
	data := make([]byte, int(keep))
	if keep > 0 {
		if _, err := writer.file.ReadAt(data, writer.size-keep); err != nil && err != io.EOF {
			return err
		}
	}
	return writer.replace(data)
}

// replace publishes a compacted tail through rename. A concurrent reader then
// sees either the complete old file or the complete new one; truncating the
// live inode in place could otherwise expose a short, zero-filled snapshot.
func (writer *durableTailWriter) replace(data []byte) (returnErr error) {
	temporary, err := os.CreateTemp(filepath.Dir(writer.path), "."+filepath.Base(writer.path)+"-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	renamed := false
	defer func() {
		if returnErr != nil {
			_ = temporary.Close()
		}
		if !renamed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if len(data) > 0 {
		written, err := temporary.Write(data)
		if err != nil {
			return err
		}
		if written != len(data) {
			return io.ErrShortWrite
		}
	}
	if err := os.Rename(temporaryPath, writer.path); err != nil {
		return err
	}
	renamed = true
	old := writer.file
	writer.file = temporary
	writer.size = int64(len(data))
	_ = old.Close()
	return nil
}

func (writer *durableTailWriter) Close() error {
	if writer == nil || writer.closed {
		return nil
	}
	writer.closed = true
	if writer.writeErr == nil {
		if err := writer.compact(); err != nil {
			writer.noteWriteError(fmt.Errorf("compact durable output: %w", err))
		}
	}
	if writer.file != nil {
		if err := writer.file.Close(); err != nil {
			writer.noteWriteError(fmt.Errorf("close durable output: %w", err))
		}
	}
	writer.stored = min(writer.size, writer.limit)
	writer.discarded = writer.received - writer.stored
	return writer.writeErr
}
