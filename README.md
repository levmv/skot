# Skot

Skot is a terminal agent for working with local files, tools, and long-running
processes. It can be used as a persistent interactive assistant or as a
one-shot command in scripts. Sessions, tool results, and job state are kept
locally.

The project is intentionally a concrete coding tool rather than a general
agent framework or workflow engine. It is currently pre-v1 and supports Linux
and macOS.

## Highlights

- A terminal UI with streaming replies, Markdown, compact tool output, queued
  follow-up input, and normal terminal scrollback.
- Local resumable sessions with replay and context compaction.
- Workspace file tools, Bash, managed jobs, web fetch, and optional web search.
- Durable supervision for explicitly background or detached work, including
  recovery of output and results after an unexpected Skot restart.
- Exact tool profiles and executable-backed custom tools for exposing narrower
  operations than unrestricted Bash.
- DeepSeek, OpenAI, OpenRouter, and local Ollama models.
- Filesystem isolation for model-owned processes, enabled by default with an
  explicit opt-out.

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
ephemeral unless `-save-session` or `-journal` is used.

```sh
sk -save-session "fix the failing tests"
sk resume                         # latest session for this workspace
sk resume 01k2                    # ID or unambiguous prefix
sk resume 01k2 "continue the fix"
```

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
parallel safety, background behavior, yield time, and detach behavior.

## Processes and jobs

Short Bash commands use a direct foreground path. A command still running after
about ten seconds returns a job ID so the model can inspect, wait for, or stop
it.

Explicit background Bash and background or detached custom tools use a separate
worker that owns their timeout, bounded log, and final result. If the main Skot
process disappears unexpectedly, resuming the session can adopt that work. A
clean exit stops non-detached jobs; detached custom tools continue across it.

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

## Scripts and JSON output

`-json` writes one versioned result object to stdout; progress and diagnostics
remain on stderr:

```sh
sk -json -save-session "fix the failing tests" > result.json
```

The result includes the reply, status, token usage, wall-clock duration, model,
reasoning effort, tool profile, total model attempts, and run identifiers, plus
an error or detached job IDs when applicable. Exit codes distinguish invalid
or incomplete runs (`2`), provider failures (`3`), and interruption (`130`)
from success (`0`) and other failures (`1`).

Run `sk -help` for the full CLI reference.

## Credits

Special thanks to [Valerii Ishchenko](https://github.com/valeriyischenko) for
his substantial contributions to the project's design and implementation.

Skot is distributed under the repository's [MIT License](LICENSE).
