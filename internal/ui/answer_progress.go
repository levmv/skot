package ui

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/levmv/skot/agent"
)

const answerProgressInterval = 80 * time.Millisecond

// AnswerProgress streams a one-shot run into ordinary terminal history. It
// shares the interactive transcript's compact tool groups and word-wrapped
// Markdown. Only the active tool tail and status remain temporary; completing
// the run does not replace the text the user has already started reading.
type AnswerProgress struct {
	mu        sync.Mutex
	size      func() (width, height int)
	now       func() time.Time
	startedAt time.Time
	source    transcriptState
	view      screenModel
	// Source snapshots, independent of wrapped rows, delimit committed output.
	committed     []progressBlockSnapshot
	presented     []progressBlockSnapshot
	renderer      *inlineRenderer
	notice        string
	dirty         bool
	elapsed       string
	width, height int
	finished      bool
	stop          chan struct{}
	stopped       chan struct{}
}

// NewAnswerProgress returns nil unless out is a terminal with styled output.
// Callers must not write to the same terminal until Finish returns.
func NewAnswerProgress(out io.Writer, root string) *AnswerProgress {
	file, ok := out.(*os.File)
	if !ok || !IsTerminalFile(file) || !shouldUseStyle(out) {
		return nil
	}
	size := func() (int, int) {
		width, height, err := term.GetSize(file.Fd())
		if err != nil {
			return 0, 0
		}
		return width, height
	}
	if width, height := size(); width <= 0 || height <= 0 {
		return nil
	}
	progress := newAnswerProgress(out, size, time.Now, root)
	progress.stop = make(chan struct{})
	progress.stopped = make(chan struct{})
	go progress.run()
	return progress
}

func newAnswerProgress(out io.Writer, size func() (int, int), now func() time.Time, root string) *AnswerProgress {
	// A one-shot run never reads stdin to query the palette or enable input modes.
	view := screenModel{useStyle: true, displayProfile: DisplayCompact, suppressToolHint: true, transcript: transcriptState{root: root}}
	view.applyTerminalTheme(false)
	renderer := newInlineRenderer(out)
	renderer.outputOnly = true
	return &AnswerProgress{
		size: size, now: now, startedAt: now(), view: view, source: transcriptState{root: root},
		renderer: renderer, dirty: true,
	}
}

func (p *AnswerProgress) run() {
	defer close(p.stopped)
	ticker := time.NewTicker(answerProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			// A failed write surfaces again when the run finishes.
			_ = p.redraw()
		}
	}
}

// Emit records events; the next tick coalesces streamed deltas into a frame.
func (p *AnswerProgress) Emit(event agent.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	switch event.Kind {
	case agent.EventModelAttemptStarted:
		p.notice = ""
		p.source.startAttempt(event.AttemptID)
	case agent.EventTextDelta:
		p.source.appendAssistant(event.AttemptID, event.Text)
	case agent.EventModelAttemptDiscarded:
		// Already printed text may be in scrollback. Preserve it with an explicit
		// correction instead of clearing the terminal to retract a failed attempt.
		for _, block := range p.source.blocks {
			if block.kind == screenBlockAssistant && block.attemptID == event.AttemptID && strings.TrimSpace(block.text) != "" {
				p.source.addBlock(screenBlockSystem, "Previous response interrupted.")
				break
			}
		}
	case agent.EventModelRetryScheduled:
		p.notice = event.Text
	case agent.EventToolStarted:
		if event.Call == nil {
			return
		}
		p.source.addToolCallAt(*event.Call, p.now())
	case agent.EventToolFinished, agent.EventToolRejected:
		// Rejected calls have no ToolStarted event.
		if event.Kind == agent.EventToolRejected && event.Call != nil {
			p.source.addToolCallAt(*event.Call, p.now())
		}
		if event.Result != nil {
			p.source.finishTool(*event.Result)
		}
	default:
		return
	}
	p.dirty = true
}

func (p *AnswerProgress) redraw() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return nil
	}
	return p.render(false)
}

// render runs under mu. Text can grow into native scrollback; tool groups are
// folded before their mutable rows would cross the visible viewport boundary.
func (p *AnswerProgress) render(finishing bool) error {
	width, height := p.size()
	if width <= 0 || height <= 0 {
		return nil
	}
	elapsed := formatTurnDuration(p.now().Sub(p.startedAt))
	if !finishing && !p.dirty && elapsed == p.elapsed && width == p.width && height == p.height {
		return nil
	}
	p.view.width, p.view.height = width, height
	if p.dirty {
		p.projectTranscript()
	}
	p.view.transcript.refresh(p.view.contentWidth(), p.view.renderBlockLinesAt)
	for {
		start, _, ok := p.view.transcript.modelOwnedToolTail()
		if !ok {
			break
		}
		startRow := 0
		if start > 0 {
			startRow = p.view.transcript.renderCache[start-1].end
		}
		if len(p.view.transcript.lines)-startRow <= max(1, height-3) || !p.view.transcript.foldOldestToolTailBlock() {
			break
		}
		p.view.transcript.refresh(p.view.contentWidth(), p.view.renderBlockLinesAt)
	}
	var dynamic []string
	if !finishing {
		if !transcriptEndsBlank(p.view.transcript.lines) {
			dynamic = append(dynamic, "")
		}
		label := "Working"
		if p.notice != "" {
			label = sanitizeTerminalText(p.notice)
		}
		status := compactSingleLine(label, p.view.contentWidth()) + " (" + elapsed + " · ctrl+c to interrupt)"
		dynamic = append(dynamic, p.view.marked(" ", p.view.mutedStyle.Render(truncateANSI(status, p.view.contentWidth()))))
	}
	err := p.renderer.RenderFrame(inlineFrame{
		transcript: p.view.transcript.lines, dynamic: dynamic,
		transcriptChanged: p.view.transcript.dirty, transcriptDirtyFrom: p.view.transcript.dirtyFrom,
	}, width, height)
	if errors.Is(err, errInlineHistoryChange) {
		// Row offsets are no longer trustworthy. Commit exactly the source
		// shown by the last successful frame, including when new events arrived
		// since that frame, then render only content the user has not seen.
		if err := p.renderer.commitOutput(); err != nil {
			return err
		}
		p.committed = p.presented
		p.dirty = true
		return p.render(finishing)
	}
	if err == nil {
		p.presented = snapshotProgressBlocks(p.source.blocks)
		p.view.transcript.presented()
		p.dirty = false
		p.elapsed, p.width, p.height = elapsed, width, height
	}
	return err
}

// Finish reconciles the final answer, folds the last tool group, and removes
// the status. Unchanged streamed text stays in place without being repeated.
func (p *AnswerProgress) Finish(answer string) error {
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		return nil
	}
	p.finished = true
	p.mu.Unlock()
	if p.stop != nil {
		close(p.stop)
		<-p.stopped
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finishAnswer(answer)
	p.source.settleAssistant()
	p.source.collapseToolTail()
	if len(p.source.blocks) == 0 && p.width == 0 {
		return p.renderer.Stop()
	}
	p.dirty = true
	if err := p.render(true); err != nil {
		_ = p.renderer.Stop()
		return err
	}
	return p.renderer.Stop()
}

// Only source text and completion state are needed to distinguish new output
// from already printed output. Do not retain mutable tool pointers here.
type progressBlockSnapshot struct {
	text     string
	toolDone bool
}

func snapshotProgressBlocks(blocks []screenBlock) []progressBlockSnapshot {
	snapshot := make([]progressBlockSnapshot, len(blocks))
	for index, block := range blocks {
		snapshot[index].text = block.text
		if block.streaming {
			// Markdown held back for text still to come is not printed yet.
			snapshot[index].text = block.text[:markdownShown(block.text)]
		}
		snapshot[index].toolDone = block.tool != nil && block.tool.done
	}
	return snapshot
}

// projectTranscript gives the shared compact renderer only the uncommitted
// source. Old blocks remain in source so late tool results and continued text
// still resolve to their original calls and attempts after a resize.
func (p *AnswerProgress) projectTranscript() {
	blocks := make([]screenBlock, 0, len(p.source.blocks))
	for index, block := range p.source.blocks {
		if index < len(p.committed) {
			previous := p.committed[index]
			switch {
			case block.kind == screenBlockAssistant:
				if !strings.HasPrefix(block.text, previous.text) {
					continue
				}
				block.text = strings.TrimPrefix(block.text, previous.text)
				block.printedPrefix = previous.text
				if strings.TrimSpace(block.text) == "" {
					continue
				}
			case block.tool != nil && block.tool.done && !previous.toolDone:
				// The call itself is already frozen in history. Report its late
				// outcome once without replaying that call or the old tool group.
				if block.tool.failed {
					block = screenBlock{kind: screenBlockError, text: block.tool.name + " failed: " + compactSingleLine(sanitizeTerminalText(block.tool.resultText), 180)}
				} else {
					block = screenBlock{kind: screenBlockSystem, text: block.tool.name + " completed."}
				}
			default:
				continue
			}
		}
		blocks = append(blocks, block)
	}
	p.view.transcript.blocks = blocks
	p.view.transcript.invalidate()
}

// Preserve already printed source, but never drop a differing final answer:
// stream events are provisional and may omit content from the accepted result.
func (p *AnswerProgress) finishAnswer(answer string) {
	answer = sanitizeTerminalText(answer)
	if strings.TrimSpace(answer) == "" {
		return
	}
	for index := len(p.source.blocks) - 1; index >= 0; index-- {
		block := &p.source.blocks[index]
		if block.kind != screenBlockAssistant || block.attemptID != p.source.currentAttempt {
			continue
		}
		if strings.HasPrefix(answer, block.text) || index >= len(p.presented) {
			block.text = answer
		} else if strings.TrimSpace(answer) != strings.TrimSpace(block.text) {
			p.source.addBlock(screenBlockSystem, "Final answer:")
			p.source.appendBlock(screenBlock{kind: screenBlockAssistant, text: answer, attemptID: p.source.currentAttempt})
		}
		return
	}
	p.source.finishAssistant(answer)
}
