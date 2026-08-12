package tools

import (
	"os/exec"
	"testing"
	"time"
)

func BenchmarkProcessLifecycle(b *testing.B) {
	cases := []struct {
		name    string
		command string
	}{
		{name: "true", command: "true"},
		{name: "small-output", command: "printf 'hello world\\n'"},
		{name: "16MiB-output", command: "head -c 16777216 /dev/zero"},
	}
	for _, test := range cases {
		b.Run("direct/"+test.name, func(b *testing.B) {
			benchmarkProcessLifecycle(b, false, test.command)
		})
		b.Run("supervised/"+test.name, func(b *testing.B) {
			benchmarkProcessLifecycle(b, true, test.command)
		})
	}
}

func benchmarkProcessLifecycle(b *testing.B, supervised bool, command string) {
	root := b.TempDir()
	manager, err := NewProcessManager(root, b.TempDir(), b.TempDir(), SandboxOff)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = manager.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		job, err := manager.start(processSpec{
			command:    command,
			workdir:    root,
			timeout:    time.Minute,
			origin:     processOriginModel,
			sessionID:  "process-lifecycle-benchmark",
			supervised: supervised,
			build: func(string) (*exec.Cmd, error) {
				process := exec.Command("bash", "-lc", command)
				process.Dir = root
				return process, nil
			},
		})
		if err != nil {
			b.Fatal(err)
		}
		<-job.done
		if supervised {
			if err := manager.refreshSupervisedJob(job); err != nil {
				b.Fatal(err)
			}
		}
		_, _ = manager.jobOutput(job, defaultCommandPreview)
		if supervised {
			manager.MarkCompletionDelivered(job.id)
			removed, err := removeSettledJobState(job)
			if err != nil {
				b.Fatal(err)
			}
			if !removed {
				b.Fatal("terminal supervised state was not removed")
			}
		}
		manager.forget(job)
	}
}
