//go:build darwin || linux

package tools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

func TestKnownJobRejectsReplacedControlFIFO(t *testing.T) {
	for _, replacementHasReader := range []bool{false, true} {
		name := "owned, replacement without reader"
		if replacementHasReader {
			name = "adopted, replacement with reader"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			manager := newProcessManagerForTest(t, t.TempDir(), home)
			pidFile := filepath.Join(t.TempDir(), "pids")
			started := runProcessResultContext(t, agent.WithToolSessionID(t.Context(), "fifo-replacement"), manager.bash, bashArgs{
				Command: workerPayloadPIDCommand(pidFile), Background: true,
			})
			job := manager.get(jobIDFromText(t, started.Content.Text()))
			workerPID, payloadPID := waitForWorkerPayloadPIDs(t, pidFile)
			defer func() {
				_ = killProcessGroupForTest(workerPID)
				_ = killProcessGroupForTest(payloadPID)
			}()
			if replacementHasReader {
				adopter := newProcessManagerForTest(t, t.TempDir(), home)
				if err := adopter.AttachSession("fifo-replacement"); err != nil {
					t.Fatal(err)
				}
				job, manager = adopter.get(job.id), adopter
				if job == nil {
					t.Fatal("job was not adopted")
				}
			}
			control := jobControlPath(job.jobDir)
			original := control + ".original"
			if err := os.Rename(control, original); err != nil {
				t.Fatal(err)
			}
			restored := false
			restore := func() {
				if restored {
					return
				}
				restored = true
				_ = os.Remove(filepath.Join(job.jobDir, jobResultFile))
				_ = os.Remove(control)
				if err := os.Rename(original, control); err != nil {
					t.Error(err)
				}
			}
			defer restore()
			replacement, err := createJobControl(control)
			if err != nil {
				t.Fatal(err)
			}
			defer replacement.Close()
			if !replacementHasReader {
				if err := replacement.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if live, err := manager.probeSupervisedJob(job); !errors.Is(err, errJobControlReplaced) || live {
				t.Fatalf("replacement reported worker lifecycle: live=%v, err=%v", live, err)
			}
			now := time.Now().UTC()
			if err := writeJSONAtomic(filepath.Join(job.jobDir, jobResultFile), jobTerminalResult{
				Version: jobProtocolVersion, JobID: job.id, Status: ProcessCompleted,
				Started: true, StartedAt: now, FinishedAt: now,
			}, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.stop(t.Context(), job.id, "test stop"); !errors.Is(err, errJobControlReplaced) {
				t.Fatalf("stop trusted replaced control or forged result: %v", err)
			}
			if status, ok := manager.Status(job.id); !ok || status.Status != ProcessRunning {
				t.Fatalf("replacement settled a live job: %+v, %v", status, ok)
			}
			restore()
			if _, err := manager.stop(t.Context(), job.id, "test cleanup"); err != nil {
				t.Fatalf("original control no longer usable: %v", err)
			}
		})
	}
}
