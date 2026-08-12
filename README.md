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
  processes are filesystem-isolated by default.
- **Narrow capabilities** — local executables can be exposed as typed tools
  instead of unrestricted Bash.
- **Delegation** — read-only child agents work in parallel without filling the
  main conversation with their traces.
- **Providers** — DeepSeek, OpenAI, OpenRouter, and Ollama.

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
sk -model openai/gpt-5 "review the current diff"

ollama pull qwen3:8b
sk -model ollama/qwen3:8b "inspect this project"
```

DeepSeek, OpenAI, and OpenRouter use `DEEPSEEK_API_KEY`, `OPENAI_API_KEY`, and
`OPENROUTER_API_KEY` respectively. Ollama needs no key and defaults to its local
OpenAI-compatible endpoint. `/model` lists and switches models interactively.

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
  "agent_models": ["openai/gpt-5-mini"]
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

Job supervision is not a container boundary. It manages ordinary Unix process
groups, so stronger whole-subtree containment remains the responsibility of an
outer container or another system-level mechanism. On a timeout or explicit
stop, structured results report how many live processes Skot observed in the
managed group; this count cannot include descendants which previously left the
group.

## Sandbox

`-sandbox auto` is the default and resolves to one concrete policy. On a host it
selects `workspace`: Landlock on Linux or Seatbelt on macOS limits model-owned
processes to the workspace, system runtime files, and a disposable synthetic
home. Inside a detected container it selects `masked`: the container remains
the main boundary, while selected paths are inaccessible to model-owned tools.
`off` is never selected automatically; it must be requested explicitly.

The same values are available interactively through `/sandbox`:

- `auto` — `workspace` on a host, `masked` in a detected container;
- `workspace` — workspace-oriented filesystem isolation plus protected paths;
- `masked` — ambient filesystem authority except for protected paths;
- `off` — ambient filesystem authority, selected explicitly.

If the selected boundary cannot be installed or fails its
read/truncate/remove probe, Skot stops instead of silently falling back to
`off`. The startup header shows the effective policy, for example
`sandbox: workspace (auto)` or `sandbox: masked (auto, docker)`.

On Linux, `workspace` and `masked` require a kernel exposing Landlock ABI V3 so file
truncation is part of the boundary. If Landlock is unavailable or disabled,
the default fails closed and the error explains how to select `off`
explicitly.

`workspace` uses a per-workspace synthetic `HOME` under the platform user cache
and keeps `TMPDIR` inside it. `masked` and `off` preserve the ordinary `HOME`.
All policies inherit network access. Skot also removes its settings and known
provider keys from model-owned process environments, but environment filtering
alone is not a security boundary. Explicit user `!` shell commands retain the
user's normal permissions.

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

Landlock has no deny rules. On Linux, if a protected path is inside a writable
tree, existing unprotected sibling subtrees remain writable, but an ancestor
that contains the protected path cannot receive broad create/list grants.
Creating a new direct sibling at that ancestor, or listing it from model-owned
Bash, may therefore be denied. Built-in file tools do not have this limitation.

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
