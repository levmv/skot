package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/levmv/skot/agent"
)

func TestJobActionsShowCommandWhenAvailable(t *testing.T) {
	const jobID = "job-a1b2c3d4e5f6"
	for _, test := range []struct {
		name            string
		command         string
		launch          *agent.ToolCall
		statusAvailable bool
	}{
		{
			name: "legacy bash launch", command: "go test ./...",
			launch: &agent.ToolCall{ID: "launch", Name: "bash", RawArguments: `{"command":"go test ./...","background":true}`},
		},
		{
			name: "program launch", command: "build_project",
			launch: &agent.ToolCall{ID: "launch", Name: "build_project", RawArguments: `{}`},
		},
		{name: "runtime", command: "go test ./...", statusAvailable: true},
		{name: "result only", command: "go test ./..."},
		{name: "unknown"},
	} {
		for _, action := range []string{"wait", "output", "stop"} {
			t.Run(test.name+"/"+action, func(t *testing.T) {
				process := agent.ProcessResult{JobID: jobID, Command: test.command, Status: agent.ProcessRunning, DurationMillis: 42000}
				fake := &fakeAgent{}
				if test.statusAvailable {
					fake.status = []agent.Detail{processDetailForTest(t, process)}
					fake.statusFound = true
				}
				model := testScreenModel(t, fake)
				if test.launch != nil {
					model.addToolCall(*test.launch)
					legacy := process
					legacy.Command = ""
					model.finishTool(agent.ToolResult{CallID: test.launch.ID, Details: []agent.Detail{processDetailForTest(t, legacy)}})
				}
				call := agent.ToolCall{ID: "inspect", Name: "job", RawArguments: fmt.Sprintf(`{"action":%q,"job_id":%q}`, action, jobID)}
				model.addToolCallAt(call, time.Now())
				assertDisplay := func(wantCommand string) {
					t.Helper()
					index := len(model.transcript.blocks) - 1
					block := model.transcript.blocks[index]
					for _, profile := range []string{DisplayCompact, DisplayDetailed, DisplayFull} {
						model.displayProfile = profile
						rendered := strings.Join(model.renderBlockLinesAt(index, block), "\n")
						want := wantCommand
						if profile == DisplayFull || want == "" {
							want = jobID
						}
						if !strings.Contains(rendered, action) || !strings.Contains(rendered, want) {
							t.Fatalf("%s missed job action or identity %q: %q", profile, want, rendered)
						}
						if profile != DisplayFull && wantCommand != "" && strings.Contains(rendered, jobID) {
							t.Fatalf("%s exposed an unnecessary job ID: %q", profile, rendered)
						}
					}
				}
				beforeResult := ""
				if test.launch != nil || test.statusAvailable {
					beforeResult = test.command
				}
				assertDisplay(beforeResult)
				if action == "stop" {
					process.Status = agent.ProcessKilled
				}
				result := agent.ToolResult{CallID: call.ID, Details: []agent.Detail{processDetailForTest(t, process)}}
				model.finishTool(result)
				assertDisplay(test.command)

				// A resumed transcript may retain the inspection without its original launch.
				model = testScreenModel(t, &fakeAgent{state: agent.State{Items: []agent.Item{
					{Kind: agent.ItemToolCall, ToolCall: &call},
					{Kind: agent.ItemToolResult, ToolResult: &result},
				}}})
				assertDisplay(test.command)
			})
		}
	}
}

func TestProcessCompletionShowsCommandAndDiagnosticsLiveAndInHistory(t *testing.T) {
	for _, process := range []agent.ProcessResult{
		{JobID: "job-completed", Command: "go test ./...", Status: agent.ProcessCompleted, DurationMillis: 42000},
		{JobID: "job-failed", Command: "build_project", Status: agent.ProcessNotStarted, Error: "start process: missing executable"},
		{JobID: "job-output", Command: "go test ./...", Status: agent.ProcessCompleted, OutputError: "read stdout: permission denied"},
	} {
		t.Run(process.JobID, func(t *testing.T) {
			details := []agent.Detail{processDetailForTest(t, process)}
			text := "Background job " + process.JobID + " completed: status=" + process.Status
			live := testScreenModel(t, &fakeAgent{})
			live.applyAgentEvent(agent.Event{Kind: agent.EventBoundaryDelivered, Text: text, Details: details})
			restored := testScreenModel(t, &fakeAgent{state: agent.State{Items: []agent.Item{
				{Kind: agent.ItemBoundaryText, Text: text, Details: details},
			}}})
			for _, model := range []*screenModel{&live, &restored} {
				index := len(model.transcript.blocks) - 1
				block := model.transcript.blocks[index]
				for _, profile := range []string{DisplayCompact, DisplayDetailed, DisplayFull} {
					model.displayProfile = profile
					rendered := strings.Join(model.renderBlockLinesAt(index, block), "\n")
					if profile == DisplayFull {
						if !strings.Contains(rendered, text) {
							t.Fatalf("full display lost the original notice: %q", rendered)
						}
						continue
					}
					for _, want := range []string{process.Command, strings.ReplaceAll(process.Status, "_", " "), process.Error, process.OutputError} {
						if !strings.Contains(rendered, want) {
							t.Fatalf("%s missed completion detail %q: %q", profile, want, rendered)
						}
					}
					if strings.Contains(rendered, process.JobID) {
						t.Fatalf("%s exposed an unnecessary job ID: %q", profile, rendered)
					}
				}
			}
		})
	}
}
