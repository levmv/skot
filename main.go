package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/app"
	productlimits "github.com/levmv/skot/internal/limits"
	"github.com/levmv/skot/internal/ui"
	workspacetools "github.com/levmv/skot/tools"
)

type cliConfig struct {
	modelURI          string
	reasoningEffort   string
	modelAPI          string
	baseURL           string
	contextWindow     int
	retryBudget       string
	streamIdleTimeout string
	maxToolIterations string
	systemPrompt      string
	systemPromptFile  string
	toolsFile         string
	home              string
	journalPath       string
	root              string
	profile           string
	saveSession       bool
	sandbox           string
	theme             string
	verbose           bool
	jsonOutput        bool
	showVersion       bool
}

type cliInvocation struct {
	resume        bool
	sessionPrefix string
	args          []string
}

func main() {
	if workspacetools.RunSandboxChildIfRequested() {
		return
	}
	if workspacetools.RunJobWorkerIfRequested() {
		return
	}
	workspacetools.HardenSupervisor()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "sk: %v\n", err)
		os.Exit(exitCodeFor(err))
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (returnErr error) {
	defaultHome := strings.TrimSpace(os.Getenv("SK_HOME"))
	if resolved, err := app.ResolveHome(defaultHome); err == nil {
		// Resolution is best-effort until flags are parsed so `sk -version` does
		// not depend on a usable home directory. app.Open validates it for every
		// invocation that actually needs local data.
		defaultHome = resolved
	}
	flags := flag.NewFlagSet("sk", flag.ContinueOnError)
	flags.SetOutput(stderr)
	config := cliConfig{}
	flags.StringVar(&config.modelURI, "model", envOr("SK_MODEL", app.DefaultModelURI), "model in provider/model form")
	flags.StringVar(&config.reasoningEffort, "reasoning-effort", strings.TrimSpace(os.Getenv("SK_REASONING_EFFORT")), "model reasoning effort; the accepted values depend on the route, for example default, off, high, or max")
	flags.StringVar(&config.modelAPI, "model-api", strings.TrimSpace(os.Getenv("SK_MODEL_API")), "override model API for every selected model (implemented: chat_completions, responses, anthropic_messages)")
	flags.StringVar(&config.baseURL, "base-url", strings.TrimSpace(os.Getenv("SK_BASE_URL")), "override provider API base URL")
	flags.IntVar(&config.contextWindow, "context-window", 0, "override model context window in tokens (0 uses model metadata)")
	flags.StringVar(&config.retryBudget, "retry-budget", strings.TrimSpace(os.Getenv("SK_RETRY_BUDGET")), fmt.Sprintf("wall-clock budget for one logical model request, attempts included (default %s)", app.DefaultRetryBudget))
	flags.StringVar(&config.streamIdleTimeout, "stream-idle-timeout", strings.TrimSpace(os.Getenv("SK_STREAM_IDLE_TIMEOUT")), fmt.Sprintf("maximum silence between model stream payloads (default %s)", app.DefaultStreamIdleTimeout))
	flags.StringVar(&config.maxToolIterations, "max-tool-iterations", strings.TrimSpace(os.Getenv("SK_MAX_TOOL_ITERATIONS")), fmt.Sprintf("maximum completed model-to-tool cycles per run, or unlimited (default %d)", agent.DefaultMaxToolIterations))
	flags.StringVar(&config.systemPrompt, "system-prompt", os.Getenv("SK_SYSTEM_PROMPT"), "system instructions")
	flags.StringVar(&config.systemPromptFile, "system-prompt-file", strings.TrimSpace(os.Getenv("SK_SYSTEM_PROMPT_FILE")), "system instructions from a file")
	flags.StringVar(&config.toolsFile, "tools", strings.TrimSpace(os.Getenv("SK_TOOLS")), "external program tool catalog (default: tools.json in the Skot data directory)")
	flags.StringVar(&config.home, "home", defaultHome, "Skot data directory for settings, credentials, sessions, and tools")
	flags.StringVar(&config.journalPath, "journal", "", "JSONL session journal to keep and resume")
	flags.StringVar(&config.root, "root", envOr("SK_ROOT", "."), "workspace root for file tools")
	flags.StringVar(&config.profile, "profile", envOr("SK_PROFILE", app.ProfileFull), "model tool profile name")
	flags.BoolVar(&config.saveSession, "save-session", false, "keep a resumable session for a one-shot invocation")
	flags.StringVar(&config.sandbox, "sandbox", envOr("SK_SANDBOX", app.SandboxAuto), "model filesystem isolation: auto, workspace, masked, or off")
	flags.StringVar(&config.theme, "theme", envOr("SK_THEME", ui.ThemeAuto), "terminal theme: auto, light, or dark")
	flags.BoolVar(&config.verbose, "v", false, "show model attempts and status")
	flags.BoolVar(&config.jsonOutput, "json", false, "emit one versioned JSON result on stdout")
	flags.BoolVar(&config.showVersion, "version", false, "print the Skot version and exit")
	flags.Usage = func() { writeCLIUsage(flags) }
	if err := flags.Parse(args); err != nil {
		return agent.MarkInvalidRequest(err)
	}
	parsedOffset := len(args) - flags.NArg()
	explicitPrompt := parsedOffset > 0 && args[parsedOffset-1] == "--"
	if config.showVersion {
		_, err := fmt.Fprintf(stdout, "sk %s\n", version)
		return err
	}
	retryBudget, err := parsePositiveDuration(config.retryBudget, "retry budget")
	if err != nil {
		return agent.MarkInvalidRequest(err)
	}
	streamIdleTimeout, err := parsePositiveDuration(config.streamIdleTimeout, "stream idle timeout")
	if err != nil {
		return agent.MarkInvalidRequest(err)
	}
	maxToolIterations, err := parsePositiveIntOrUnlimited(config.maxToolIterations, "max tool iterations")
	if err != nil {
		return agent.MarkInvalidRequest(err)
	}
	invocation := parseInvocation(flags.Args(), explicitPrompt)
	if strings.TrimSpace(config.systemPromptFile) != "" {
		if strings.TrimSpace(config.systemPrompt) != "" {
			return agent.MarkInvalidRequest(errors.New("set system instructions with -system-prompt or -system-prompt-file, not both"))
		}
		config.systemPrompt, err = loadPromptFile(config.systemPromptFile, "system prompt")
		if err != nil {
			return agent.MarkInvalidRequest(err)
		}
	}
	setFlags := make(map[string]bool)
	flags.Visit(func(value *flag.Flag) { setFlags[value.Name] = true })
	modelExplicit := setFlags["model"] || strings.TrimSpace(os.Getenv("SK_MODEL")) != ""
	reasoningEffortExplicit := setFlags["reasoning-effort"] || strings.TrimSpace(os.Getenv("SK_REASONING_EFFORT")) != ""
	profileExplicit := setFlags["profile"] || strings.TrimSpace(os.Getenv("SK_PROFILE")) != ""
	sandboxExplicit := setFlags["sandbox"] || strings.TrimSpace(os.Getenv("SK_SANDBOX")) != ""
	config.theme, err = ui.NormalizeTheme(config.theme)
	if err != nil {
		return agent.MarkInvalidRequest(err)
	}
	inFile, outFile, terminalScreen := ui.CanUseScreen(stdin, stdout)
	interactive := len(invocation.args) == 0 && terminalScreen
	var prompt string
	if !interactive {
		prompt, err = readPrompt(invocation.args, stdin)
		if err != nil {
			return agent.MarkInvalidRequest(err)
		}
	}

	application, err := app.Open(ctx, app.Config{
		Home: config.home, Root: config.root,
		ModelURI: config.modelURI, ReasoningEffort: config.reasoningEffort, ModelAPI: config.modelAPI,
		ModelExplicit: modelExplicit, ReasoningEffortExplicit: reasoningEffortExplicit,
		BaseURL: config.baseURL, ContextWindow: config.contextWindow,
		RetryBudget: retryBudget, StreamIdleTimeout: streamIdleTimeout, MaxToolIterations: maxToolIterations, SystemPrompt: config.systemPrompt,
		ToolsFile: config.toolsFile,
		Profile:   config.profile, ProfileExplicit: profileExplicit,
		Sandbox: config.sandbox, SandboxExplicit: sandboxExplicit,
		JournalPath: config.journalPath, Resume: invocation.resume, ResumePrefix: invocation.sessionPrefix,
		SaveSession: config.saveSession, Interactive: interactive,
	})
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, application.Close()) }()
	for _, notice := range application.StartupNotices() {
		fmt.Fprintln(stderr, "sk: "+notice)
	}
	if interactive {
		return ui.RunScreen(ctx, application, ui.Config{
			ModelURI:        application.CurrentModel(),
			ReasoningEffort: application.CurrentReasoningEffort(),
			Root:            application.Root(),
			Profile:         application.CurrentProfile(),
			Security:        application.SecuritySummary(),
			Theme:           config.theme,
		}, inFile, outFile)
	}
	measureUsage := config.verbose || config.jsonOutput
	var usageBefore agent.ModelUsage
	if measureUsage {
		state, err := application.State(ctx)
		if err != nil {
			return fmt.Errorf("read usage before run: %w", err)
		}
		usageBefore = state.Usage
	}
	var observer *runEventObserver
	var emit agent.EmitFunc
	if config.verbose || config.jsonOutput {
		observer = &runEventObserver{verbose: verboseEmitter(config.verbose, stderr)}
		emit = observer.emit
	}
	startedAt := time.Now()
	sessionWasResumable := application.SessionID() != ""
	result, runErr := application.Run(ctx, prompt, emit)
	durationMillis := time.Since(startedAt).Milliseconds()
	var usage agent.ModelUsage
	if measureUsage && (runErr == nil || result.RunID != "") {
		state, err := application.State(context.WithoutCancel(ctx))
		if err != nil {
			return errors.Join(runErr, fmt.Errorf("read usage after run: %w", err))
		}
		usage = subtractUsage(state.Usage, usageBefore)
	}
	if config.jsonOutput && (runErr == nil || result.RunID != "" || result.Answer != "") {
		reasoningEffort := application.CurrentReasoningEffort()
		if reasoningEffort == "" {
			reasoningEffort = "default"
		}
		metadata := jsonRunMetadata{
			DurationMillis:  durationMillis,
			Model:           application.CurrentModel(),
			ReasoningEffort: reasoningEffort,
			Profile:         application.CurrentProfile(),
			ModelAttempts:   int(observer.modelAttempts.Load()),
		}
		if err := writeJSONResult(stdout, result, usage, application.SessionID(), metadata, runErr); err != nil {
			return errors.Join(runErr, fmt.Errorf("write JSON result: %w", err))
		}
	} else if runErr == nil || result.Answer != "" {
		if _, err := fmt.Fprintln(stdout, result.Answer); err != nil {
			return errors.Join(runErr, fmt.Errorf("write answer: %w", err))
		}
	}
	if config.verbose && !config.jsonOutput && runErr == nil {
		fmt.Fprintf(stderr, "[usage: prompt=%d cached=%d completion=%d reasoning=%d total=%d]\n",
			usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.TotalTokens)
	}
	autoRetained := !sessionWasResumable && application.SessionID() != ""
	if (config.saveSession || len(result.DetachedJobs) != 0 || autoRetained) && application.SessionID() != "" {
		fmt.Fprintf(stderr, "Resume with: sk resume %s\n", application.ShortSessionID())
	}
	return runErr
}

func subtractUsage(total, previous agent.ModelUsage) agent.ModelUsage {
	return agent.ModelUsage{
		InputTokens:       max(0, total.InputTokens-previous.InputTokens),
		CachedInputTokens: max(0, total.CachedInputTokens-previous.CachedInputTokens),
		OutputTokens:      max(0, total.OutputTokens-previous.OutputTokens),
		ReasoningTokens:   max(0, total.ReasoningTokens-previous.ReasoningTokens),
		TotalTokens:       max(0, total.TotalTokens-previous.TotalTokens),
	}
}

func parseInvocation(args []string, explicitPrompt bool) cliInvocation {
	if explicitPrompt || len(args) == 0 || strings.ToLower(args[0]) != "resume" {
		return cliInvocation{args: args}
	}
	invocation := cliInvocation{resume: true}
	if len(args) > 1 {
		invocation.sessionPrefix = strings.TrimSpace(args[1])
		invocation.args = args[2:]
	}
	return invocation
}

func writeCLIUsage(flags *flag.FlagSet) {
	output := flags.Output()
	fmt.Fprintln(output, "Skot is a terminal agent for working with local files and tools.")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Usage:")
	fmt.Fprintln(output, "  sk [flags] [prompt...]")
	fmt.Fprintln(output, "  sk [flags] resume")
	fmt.Fprintln(output, "  sk [flags] resume <id-or-prefix> [prompt...]")
	fmt.Fprintln(output, "  sk [flags] -- [prompt...]")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "With no prompt, Skot opens a new persistent interactive session.")
	fmt.Fprintln(output, "Bare resume continues the latest session in the current workspace.")
	fmt.Fprintln(output)
	fmt.Fprintln(output, "Flags:")
	flags.PrintDefaults()
}

func readPrompt(args []string, stdin io.Reader) (string, error) {
	if len(args) != 0 {
		prompt := strings.TrimSpace(strings.Join(args, " "))
		if prompt == "" {
			return "", agent.ErrEmptyInput
		}
		return prompt, nil
	}
	if file, ok := stdin.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return "", fmt.Errorf("inspect stdin: %w", err)
		}
		if info.Mode()&os.ModeCharDevice != 0 {
			return "", errors.New("prompt is required when the interactive screen is unavailable")
		}
	}
	data, err := io.ReadAll(io.LimitReader(stdin, productlimits.MaxPipedInputBytes+1))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	if len(data) > productlimits.MaxPipedInputBytes {
		return "", fmt.Errorf("stdin exceeds %d bytes", productlimits.MaxPipedInputBytes)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", agent.ErrEmptyInput
	}
	return prompt, nil
}

func loadPromptFile(path, setting string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", setting, err)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", fmt.Errorf("%s file %s is empty", setting, path)
	}
	return prompt, nil
}

func verboseEmitter(enabled bool, output io.Writer) agent.EmitFunc {
	if !enabled {
		return nil
	}
	return func(event agent.Event) {
		switch event.Kind {
		case agent.EventModelAttemptStarted:
			fmt.Fprintf(output, "sk: model attempt %s\n", event.AttemptID)
		case agent.EventModelAttemptDiscarded:
			fmt.Fprintf(output, "sk: discarded attempt: %s\n", event.Text)
		case agent.EventModelRetryScheduled:
			fmt.Fprintf(output, "sk: %s\n", event.Text)
		case agent.EventToolStarted:
			fmt.Fprintf(output, "sk: tool %s\n", event.Call.Name)
		case agent.EventToolFinished:
			if event.Result.Error {
				fmt.Fprintf(output, "sk: tool %s failed: %s\n", event.Call.Name, event.Result.Content)
			}
		case agent.EventToolRejected:
			fmt.Fprintf(output, "sk: tool %s rejected: %s\n", event.Call.Name, event.Result.Content)
		case agent.EventStatus, agent.EventBoundaryDelivered, agent.EventContextCompacted, agent.EventToolResultsPruned:
			fmt.Fprintf(output, "sk: %s\n", event.Text)
		case agent.EventRunFinished:
			if event.ToolLimitReached {
				fmt.Fprintf(output, "sk: %s (tool iteration limit reached)\n", event.Status)
			} else {
				fmt.Fprintf(output, "sk: %s\n", event.Status)
			}
		}
	}
}

type runEventObserver struct {
	modelAttempts atomic.Int64
	verbose       agent.EmitFunc
}

func (observer *runEventObserver) emit(event agent.Event) {
	if event.Kind == agent.EventModelAttemptStarted {
		observer.modelAttempts.Add(1)
	}
	if observer.verbose != nil {
		observer.verbose(event)
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func parsePositiveDuration(value, name string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, value, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return duration, nil
}

func parsePositiveIntOrUnlimited(value, name string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if strings.EqualFold(value, "unlimited") {
		return -1, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid %s %q: use a positive integer or unlimited", name, value)
	}
	return parsed, nil
}
