# Skot

Skot is a terminal agent for working with local files, tools, and long-running
processes. It can be used as a persistent interactive assistant or as a
one-shot command in scripts. Sessions, tool results, and job state are kept
locally.

## What it does

- **Interactive** — streaming replies, Markdown, compact tool output, queued
  input, and ordinary terminal scrollback.
- **Or unattended** — a prompt from an argument or stdin, the answer on stdout,
  `-json` for one versioned result, and exit codes a caller can branch on.
- **Durable** — sessions and background jobs live on disk; resume a
  conversation later, and unfinished work is picked up after a restart.
- **Tools** — files, search, Bash, managed jobs, web fetch, and optional web
  search.
- **Bounded** — a profile names the exact tools a run may use, and model-owned
  processes are filesystem-isolated by default. This contains mistakes rather
  than a determined model; see the [security model](#security-model).
- **Narrow capabilities** — local executables can be exposed as typed tools
  instead of unrestricted Bash.
- **Delegation** — read-only child agents work in parallel without filling the
  main conversation with their traces.
- **Providers** — DeepSeek, OpenAI, OpenRouter, OpenCode Go, and Ollama.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/levmv/skot/main/install.sh | sh
```

Or build from a checkout:

```sh
cd skot
make build
```

## Quick start

Start an interactive session and use `/login` to store a provider key:

```sh
sk
```

Or provide a key through the environment and run one prompt:

```sh
export DEEPSEEK_API_KEY=...
sk "inspect the workspace and run the tests"
```

Models use `provider/model` names:

```sh
ollama pull qwen3:8b
sk -model ollama/qwen3:8b "inspect this project"
```

DeepSeek, OpenAI, OpenRouter, and OpenCode Go use `DEEPSEEK_API_KEY`,
`OPENAI_API_KEY`, `OPENROUTER_API_KEY`, and `OPENCODE_API_KEY` respectively.
Ollama needs no key and defaults to its local OpenAI-compatible endpoint.
`/model` lists and switches models interactively.

## Sessions and interactive use

Running `sk` without a prompt starts a persistent session. A one-shot run is
ephemeral unless `-save-session` or `-journal` is used, or it leaves detached
work running.

```sh
sk -save-session "fix the failing tests"
sk resume                         # latest session for this workspace
sk resume 01k2                    # ID or unambiguous prefix
sk resume 01k2 "continue the fix"
```

Session journals contain conversation and workspace data; treat them as private.

Use `/help` inside the UI for commands and keyboard shortcuts. The most useful
commands are `/resume`, `/model`, `/profile`, `/sandbox`, `/context`,
`/compact`, and `/exit` (`/quit` and `/q` also work).

Enter sends a message; Shift/Alt+Enter or Ctrl+J inserts a newline. Escape
cancels the active turn, and Alt+Up recalls queued input. `! command` runs a
shell command and includes its result in the conversation; `!! command` runs it
privately. These user-owned shell commands use your normal permissions rather
than the model sandbox.

Skot also loads applicable `AGENTS.md` instructions from the workspace root
down to the current directory.

## Tools and profiles

The built-in file tools operate inside `-root`, which defaults to the current
directory. Paths and symlinks may not escape that root; reads and searches are
bounded, and writes are atomic.

| Profile | Tools |
| --- | --- |
| `read-only` | `read`, `ls`, `grep`, `glob` |
| `edit` | read-only tools plus `edit`, `write` |
| `full` | `read`, `grep`, `glob`, `edit`, `write`, `bash`, `job` |

All default profiles can fetch public web pages. Web search is enabled when a
Tavily or Exa key is available. Select a profile with `-profile` or `/profile`;
custom exact tool lists can be added under `profiles` in Skot's `config.json`.

### Child agents

Add the single `agent` capability to a custom profile when the model should be
allowed to delegate independent work:

```json
{
  "profiles": {
    "delegate": ["read", "grep", "glob", "edit", "write", "bash", "job", "agent"]
  },
  "agent_models": ["deepseek/deepseek-v4-flash"]
}
```

The parent can start, continue, inspect, wait for, or stop child agents through
that one tool. Children use the same workspace and inherit the current model by
default, but receive a fresh conversation and only the built-in read-only tool
set; they cannot create more agents. `agent_models` is the allowlist for model
overrides and may be omitted.

Up to four children run concurrently. Their journals live under the parent
session, stay out of the normal session picker, and remain available after the
parent is resumed. A clean exit cancels unfinished child runs without discarding
their history; it does not leave model calls detached in another process. A
one-shot command which creates a child is retained automatically and prints its
resume command.

### Custom program tools

Skot can expose local executables as typed model tools through `tools.json` in
its state directory, or another file selected with `-tools`:

```json
{
  "tools": [
    {
      "name": "go_test",
      "description": "Run the Go test suite",
      "command": ["go", "test", "./..."],
      "parameters": {
        "type": "object",
        "additionalProperties": false
      },
      "timeout": 600
    }
  ]
}
```

The program receives its arguments as JSON on stdin and returns its model-facing
result on stdout. A configured tool must also be named by the active profile.
Declarations can set a working directory, environment overlay, timeout,
parallel safety, background behavior, yield time, and detach behavior. Integer
`timeout` and `yield` values are seconds.

## Processes and jobs

Short Bash commands use a direct foreground path. A command still running after
about ten seconds returns a job ID so the model can inspect, wait for, or stop
it.

Explicit background Bash and background or detached custom tools use a separate
worker that owns their timeout, bounded log, and final result. If the main Skot
process disappears unexpectedly, resuming the session can adopt that work. A
clean exit stops non-detached jobs; detached custom tools continue across it.
If a one-shot run leaves detached work running, Skot keeps that session
automatically and prints the command needed to resume it.

Job supervision uses ordinary Unix process groups, not a container boundary. A
descendant that leaves its group may escape later observation and stopping; use
an outer container when whole-process-tree containment matters.

## Security model

Skot's boundaries are designed to contain accidental damage, not an agent
deliberately trying to escape. Use a container or virtual machine when the
model or the code it runs is untrusted.

Within that threat model, three rules describe the boundary:

- a **profile** decides which tools the model has at all;
- **file tools** always work inside the workspace root, and this cannot be
  turned off;
- **model-owned processes** run under the selected sandbox policy.

### What this does not cover

- **Network access.** Model-owned processes inherit it; Skot does not filter
  egress.
- **Sandbox escapes.** Races, kernel bugs, and a determined adversary are
  outside the threat model.
- **Explicit bypasses.** `-sandbox off` and user `!` shell commands run with
  your normal filesystem permissions.
- **Workspace contents.** The agent may change anything inside the root except
  explicitly protected paths.

### Sandbox policies

- `auto` (the default) — `workspace` on a host, `masked` in a detected
  container;
- `workspace` — model-owned processes can access the workspace, required
  system runtime files, and a disposable synthetic home;
- `masked` — the container remains the main boundary; processes retain its
  filesystem access except for protected paths;
- `off` — processes retain your filesystem access and protected paths are
  disabled. It is never selected automatically.

The same policies are available interactively through `/sandbox`.

If the selected boundary cannot be installed or fails its
read/truncate/remove probe, Skot stops instead of silently falling back to
`off`. The startup header shows the effective policy, for example
`sandbox: workspace (auto)` or `sandbox: masked (auto, docker)`.

On Linux, `workspace` and `masked` require Landlock ABI V3 so truncation is part
of the boundary. If it is unavailable, the default fails closed.

`workspace` uses a per-workspace synthetic `HOME` and keeps `TMPDIR` inside it;
`masked` and `off` preserve the ordinary `HOME`. Skot removes its settings and
known provider keys from model-owned process environments, but environment
filtering is not a security boundary. User `!` commands are not model-owned and
retain the user's normal environment and permissions.

### Protected paths

Skot's state home is always protected in `workspace` and `masked`. Additional
paths can be added in Skot's `config.json`:

```json
{
  "protected_paths": [".env", "~/.ssh", "/work/shared/credentials"]
}
```

Absolute paths are used directly, `~/` is relative to the user's real home,
and other paths are relative to the workspace root. Nested and duplicate paths
are collapsed. The filesystem root is rejected. `workspace` and `masked` also
reject a protected path that contains the whole workspace (a protected child
inside the workspace is valid). Protection applies consistently to `read`,
`ls`, `grep`, `glob`, `edit`, `write`, model-owned Bash, and configured program
tools; symlink aliases do not bypass it. Protected entries are omitted by
listing and search tools, and protected `AGENTS.md` files are not added to model
instructions.

Selecting `off` disables this protection, while the ordinary workspace boundary
of the built-in file tools still remains.

Landlock is allow-list based. On Linux, protecting a child of a writable
directory can therefore prevent model-owned processes from listing that parent
or creating new siblings there. Built-in file tools do not have this
limitation.

## Scripts and unattended runs

A prompt is an argument, or stdin when no prompt argument is given. Most
settings also read an `SK_*` environment variable, so a run is easy to pin down
in a script:

```sh
SK_PROFILE=read-only sk -json "summarize what this project does" > result.json
```

`-json` writes one versioned result object to stdout, while progress and
diagnostics remain on stderr. It includes the reply, status, token usage,
wall-clock duration, model, reasoning effort, tool profile, total model
attempts, and run identifiers, plus an error or detached job IDs when
applicable. Exit codes distinguish invalid or incomplete runs (`2`), provider
failures (`3`), and interruption (`130`) from success (`0`) and other failures
(`1`).

`-max-tool-iterations` and `-retry-budget` bound how far an unattended run may
go on its own. A one-shot run is ephemeral, but `-save-session` keeps it, so a
later step can continue the same conversation:

```sh
sk -save-session "start the migration"
sk resume 01k2 "now update the tests"
```

Run `sk -help` for the full CLI reference.

## Credits

Special thanks to [Valerii Ishchenko](https://github.com/valeriyischenko) for
his substantial contributions to the project's design and implementation.

Skot is distributed under the repository's [MIT License](LICENSE).
