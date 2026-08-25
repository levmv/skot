# Skot user reference

This document describes Skot's user-facing behavior and configuration. For
installation and a short introduction, see the [project README](../README.md).

## Invocation

```text
sk [flags] [prompt...]
sk [flags] resume
sk [flags] resume <id-or-prefix> [prompt...]
sk [flags] update
sk [flags] -- [prompt...]
```

With no prompt and a terminal attached, `sk` opens a new persistent
interactive session. A prompt supplied as arguments or through stdin runs once
without the interactive screen. Bare `resume` selects the latest session for
the current workspace; an ID or unambiguous prefix selects a particular one.

`sk update` installs a newer stable GitHub release for the current platform
after verifying its SHA-256 checksum. Existing processes continue with the old
version until restarted. Development builds cannot update themselves.

Use `--` when prompt text could otherwise be parsed as a flag or begins with a
reserved command name such as `resume` or `update`:

```sh
sk -- "-review the command-line parser"
sk -- resume this discussion
```

### CLI flags and environment variables

Command-line flags override their corresponding environment variables.
Durations use Go syntax such as `30s`, `5m`, or `1h30m`.

| Flag | Environment | Meaning |
| --- | --- | --- |
| `-model provider/model` | `SK_MODEL` | Select the model route. The product default is `deepseek/deepseek-v4-flash`. |
| `-reasoning-effort value` | `SK_REASONING_EFFORT` | Select a route-supported effort such as `default`, `off`, `high`, or `max`. Accepted values depend on the route. |
| `-model-api api` | `SK_MODEL_API` | Override the protocol with `chat_completions`, `responses`, or `anthropic_messages`. |
| `-base-url url` | `SK_BASE_URL` | Override the provider API base URL. |
| `-context-window tokens` | — | Override model context metadata; `0` uses route metadata. |
| `-retry-budget duration` | `SK_RETRY_BUDGET` | Wall-clock retry budget for one logical model request. Default: `15m`. |
| `-stream-idle-timeout duration` | `SK_STREAM_IDLE_TIMEOUT` | Maximum silence between stream events. Default: `5m`. |
| `-max-tool-iterations n` | `SK_MAX_TOOL_ITERATIONS` | Emergency model-to-tool cycle limit. Default: `128`; use `unlimited` to disable it. |
| `-system-prompt text` | `SK_SYSTEM_PROMPT` | Replace the built-in system instructions. Use `{{workspace_root}}` to insert the workspace root. |
| `-system-prompt-file path` | `SK_SYSTEM_PROMPT_FILE` | Read system instructions from a file. It cannot be combined with `-system-prompt`. |
| `-root path` | `SK_ROOT` | Primary workspace and default path base for model-owned file and process tools. Default: current directory. |
| `-tools name` | `SK_TOOLS` | Select the tool set available to the model. Product default: `default`. |
| `-tools-file path` | `SK_TOOLS_FILE` | Load custom program tool definitions. Default: `tools.json` in the Skot data directory. |
| `-scope value` | `SK_SCOPE` | Select the filesystem reach of built-in file tools and model-owned processes: `workspace` or `machine`. Default: `workspace`. |
| `-add-dir path` | — | Add a directory tree to `workspace` scope. Repeat the flag to add more than one. |
| `-protect-path path` | — | Hide a path from model-owned tools for this run. Repeat the flag to protect more than one. |
| `-home path` | `SK_HOME` | Select the Skot data directory. Default: `~/.skot`. |
| `-journal path` | — | Use an explicit JSONL session journal. |
| `-save-session` | — | Retain a one-shot invocation as a resumable managed session. |
| `-json` | — | Emit one versioned JSON result on stdout. |
| `-v` | — | Show attempts, retries, tool activity, maintenance, status, and final token usage on stderr. |
| `-version` | — | Print the version and exit. |

`SK_COLOR=always|never` overrides automatic styled-output detection.
`NO_COLOR` disables styling.

For a new interactive session, an explicit flag or environment variable wins
over the current workspace preference, which wins over the model and effort last
selected in any workspace, which wins over the product default. Interactive
resume additionally restores the session-recorded model and effort between
explicit input and the workspace preference.

Headless runs never read interactive preferences. A fresh one-shot or
`-save-session` run uses only explicit CLI/environment input and product
defaults. Headless resume may restore the session-recorded model and effort;
tool-set and filesystem settings still come from the current invocation and
configuration. A new or empty `-journal` follows the fresh rule; an existing
journal with a recorded selection follows the continuation rule.

## Models and credentials

Models are addressed as `provider/model`. Use `/model` interactively to see
the currently available route list and switch models.

| Provider | Credential |
| --- | --- |
| DeepSeek | `DEEPSEEK_API_KEY` |
| Anthropic | `ANTHROPIC_API_KEY` |
| OpenAI | `OPENAI_API_KEY` |
| OpenRouter | `OPENROUTER_API_KEY` |
| OpenCode Go | `OPENCODE_API_KEY` |
| Ollama | None; defaults to the local OpenAI-compatible endpoint |

Interactive `/login [provider]` stores a key in the private credential store;
`/logout [provider]` removes it. An environment variable takes precedence over
a stored key and must be unset before the corresponding stored key can be
changed.

Web tools use separate credentials:

| Service | Credential | Capability |
| --- | --- | --- |
| Keenable | `KEENABLE_API_KEY` (optional) | Web search and fetch |
| Tavily | `TAVILY_API_KEY` | Web search |
| Exa | `EXA_API_KEY` | Web search and fetch |
| Firecrawl | `FIRECRAWL_API_KEY` | Web fetch |

Keenable is the default search provider and the first fetch backend. Without a
key, Skot uses Keenable's public endpoints and their shared per-IP limits.
`/login keenable` or the environment variable switches both operations to the
account's allowance. If Keenable fails, configured providers and the bounded
direct fetcher remain fallbacks.

### Models Skot does not list

Any `provider/model` can be entered by hand, through `Enter model URI…` in the
`/model` picker or as `/model provider/model`. Most providers speak one API, so
an unlisted model of theirs is selected like any other, with a conservative
context estimate until the route is described.

A subscription gateway such as OpenCode Go serves several APIs at once, and the
API of a model Skot does not list cannot be guessed from the provider. Selecting
one asks which API it speaks; answer with the list, or name the API directly as
`/model opencode-go/some-model chat_completions`. The answer belongs to that
model: it is remembered for later selections and for reopening the session, it
is not applied to any other route, and it is dropped once a Skot release
describes the model itself. Choosing the wrong API is not dangerous — the
request fails at the provider.

An explicit `-model-api` or `-base-url` can make a route unverified. Unlike an
answer given for one model, `-model-api` applies to every model selected in that
run. Skot retains conservative protocol defaults in either case and reports a
compatibility hint if the provider rejects the request. Route-specific features
such as a thinking switch are not carried across an incompatible protocol
override.

## Data directory and configuration

Skot uses `~/.skot` by default. `SK_HOME` and `-home` select another data
directory. It contains:

- `config.json` — global configuration: custom tool sets, child-model
  allowlist, and protected paths;
- `interactive.json` — UI state and interactive preferences managed by Skot;
- `auth.json` — credentials managed by `/login` and `/logout`;
- `tools.json` — the default custom program tool catalog;
- `sessions/` — managed session journals and child-agent state.

A missing data directory is created with mode `0700`; an existing
`-home`/`SK_HOME` directory keeps its mode. Skot-managed state files are created
with mode `0600`. Session journals contain conversation and workspace data and
should also be treated as private.

Model-owned processes in `workspace` scope use a separate disposable home under
the platform user cache, not under the Skot data directory. On Linux its root
is `$XDG_CACHE_HOME/skot/tool-home` when `XDG_CACHE_HOME` is set and otherwise
`$HOME/.cache/skot/tool-home`; each canonical workspace gets a hashed
subdirectory. Consequently `-home`/`SK_HOME` moves private Skot state but does
not isolate or relocate this shared cache. Set the platform cache environment
as well when a CI job needs a fully separate process home. A workspace cache
may be removed when no Skot invocation or surviving job/process for that
workspace is still running; Skot does not currently garbage-collect it
automatically.

A representative configuration is:

```json
{
  "tool_sets": {
    "delegate": ["read", "grep", "glob", "edit", "write", "bash", "job", "agent"]
  },
  "agent_models": ["deepseek/deepseek-v4-flash"],
  "protected_paths": [".env", "~/.ssh"]
}
```

| Field | Meaning |
| --- | --- |
| `tool_sets` | Map of tool set names to exact ordered tool-name lists. A custom definition replaces a built-in set with the same name. |
| `agent_models` | Models that the optional `agent` tool may select explicitly. |
| `protected_paths` | Paths hidden from built-in file tools and model-owned processes. Empty by default. |

Interactive theme and the model selection history are shared across workspaces.
Workspace-specific model, reasoning effort, tool set, and filesystem settings
(scope, added directories, and protected paths) are remembered per canonical
workspace path. Symlink aliases share preferences, while separate clones and
worktrees do not. Skot manages these preferences in `interactive.json`;
headless runs do not read them.

Use `/login` rather than editing `auth.json` directly.

## Sessions and interactive use

Running `sk` with no prompt creates a managed persistent session. A one-shot
run is ephemeral unless `-save-session` or `-journal` is used, it creates a
child agent, or it leaves detached work running.

Without `-save-session` or `-journal`, a one-shot run with only built-in
non-process tools keeps its conversation in memory and leaves no journal in
`SK_HOME`. A run whose tools can create durable work uses a temporary on-disk
journal; it is removed after normal completion unless the session becomes
resumable.

```sh
sk -save-session "fix the failing tests"
sk resume
sk resume 0f3a
sk resume 0f3a "continue the fix"
```

Session selection is scoped to the canonical workspace path. A short ID may be
used when it identifies exactly one session. Bare `resume` chooses the most
recent session for that workspace.

### Interactive commands

| Command | Action |
| --- | --- |
| `/help` | Show keyboard shortcuts. |
| `/clear` | Start a new session. |
| `/resume [id-or-prefix]` | Choose or resume a previous session. |
| `/login [provider]` | Store a provider or service key. |
| `/model [provider/model]` | List or switch models. |
| `/tools [name]` | Show or switch the active tool set. |
| `/scope [workspace|machine]` | Show or change the filesystem scope, added directories, and protected paths. |
| `/theme [auto|light|dark]` | Show or persist the interactive terminal theme. Default: `auto`, which asks the terminal for its background colour and falls back to `dark` when there is no answer. Set `light` or `dark` explicitly if your terminal filters that query. |
| `/context` | Show the current context budget. |
| `/compact` | Compact older completed conversation blocks. |
| `/logout [provider]` | Remove a stored key. |
| `/exit`, `/quit`, `/q` | Exit Skot. |

Enter sends a message. Shift/Alt+Enter or Ctrl+J inserts a newline. Shift+Tab
cycles the filesystem scope, including while a turn is running. Escape cancels
the active turn, Alt+Up recalls queued input, and Ctrl+C exits.

`! command` runs a shell command and includes its result in the conversation.
`!! command` runs it privately. Both are user-owned commands and therefore use
the user's normal environment and filesystem permissions rather than the
model-owned filesystem scope.

Skot loads applicable `AGENTS.md` instructions from the workspace root down
to the current directory.

## Tools and tool sets

Built-in file tools use the current filesystem scope, bound their reads and
searches, and write atomically. See [Filesystem access](#filesystem-access)
for path and scope rules.

`read` handles UTF-8 text and images. `offset` and `limit` apply only to text.
Images are reduced to at most 2000 pixels on the longer side before delivery.

| Tool set | Tools |
| --- | --- |
| `default` | `read`, `grep`, `glob`, `edit`, `write`, `bash`, `job` |
| `edit` | `read`, `ls`, `grep`, `glob`, `edit`, `write` |
| `read-only` | `read`, `ls`, `grep`, `glob` |

All built-in tool sets include bounded public `web_fetch` and `web_search`. A
custom tool set is an exact tool list, not a set of additions to a built-in set.

On Linux, `default` also includes `ls` whenever protected paths are configured.
The process boundary may make some ancestor-directory operations unavailable;
the built-in tool keeps directory listing available. Explicit custom tool sets
remain exact.

### Child agents

Add the single `agent` capability to a custom tool set to permit delegation:

```json
{
  "tool_sets": {
    "delegate": ["read", "grep", "glob", "edit", "write", "bash", "job", "agent"]
  },
  "agent_models": ["deepseek/deepseek-v4-flash"]
}
```

The parent can start, continue, inspect, wait for, or stop children through that
one tool. Children share the workspace and inherit the current model by default,
but receive a fresh conversation and only the built-in read-only tools. They
cannot create more agents. `agent_models` allowlists explicit child model
overrides and may be omitted.

Up to four children run concurrently. Their journals live under the parent
session, stay out of the normal session picker, and remain available after the
parent is resumed. A clean exit cancels unfinished child runs without discarding
their history; it does not leave model calls detached in another process.

### Custom program tools

Skot loads `tools.json` from its data directory, or another file selected by
`-tools-file`. A minimal catalog is:

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

The executable receives one JSON object on stdin. Stdout becomes the model-facing
result; non-empty stderr is captured separately and appended under a `stderr:`
header. A configured tool is visible only when the active tool set names it.
Skot validates the declaration at startup and resolves its executable when that
tool set is activated.

| Field | Meaning |
| --- | --- |
| `name` | Required tool name: starts with a letter and contains only letters, digits, and underscores. |
| `description` | Required model-facing description. |
| `command` | Required executable and fixed arguments. |
| `parameters` | Optional JSON Schema for the object sent to stdin. By default, any object is accepted. |
| `timeout` | Hard timeout in seconds. Default: `600`; maximum: `3600`. |
| `workdir` | Working directory relative to the workspace root. |
| `env` | Environment overlay. `HOME`, `TMPDIR`, and `SK_INTERNAL_*` are reserved. |
| `parallel_safe` | Allows parallel calls when true. |
| `background` | `never` (default), `auto` (adds a model-visible boolean), or `always`. |
| `yield` | Seconds to wait for a foreground result before returning a managed job. |
| `detach` | Allow managed work to survive a clean Skot exit. Requires background capability or a positive yield. |

Any tool set containing `bash` or a background-capable program tool must also
contain `job`, so the model can observe and stop work it starts.

## Processes and jobs

Short Bash commands run in the foreground. A command still running after about
ten seconds returns a job ID so the model can inspect, wait for, or stop it.

Explicit background Bash and background or detached program tools use a
separate worker that owns their timeout, bounded log, and final result. If the
main Skot process disappears unexpectedly, resuming the session can adopt that
work. A clean exit stops non-detached jobs; detached custom tools continue
across it. If a one-shot run leaves detached work running, Skot retains the
session automatically and prints its resume command.

Job supervision uses Unix process groups, not a container boundary. A
descendant that leaves its group may escape later observation and stopping; use
an outer container when whole-process-tree containment matters.

## Context management

`/context` estimates the input budget for the next model request, including
instructions, tool definitions, rolling summary, history, and queued input.
Reasoning data that will not be sent again is excluded.

When the next request would exceed its input limit, Skot first tries to prune
older tool-result bodies and then compacts an older completed prefix into a
rolling summary. Maintenance never rewrites the active run. `/compact`
requests the same compaction explicitly.

`-context-window` overrides missing or incorrect route metadata.
`-max-tool-iterations` is an emergency fuse for repeated tool cycles; when it
is reached, Skot asks the model for a final answer without offering tools.

## Filesystem access

Skot does not approve individual commands. Every tool in the active tool set
may act without confirmation. Filesystem reach is bounded structurally:

- a tool set decides which capabilities the model has;
- one selected scope controls both built-in file tools and model-owned Bash or
  program processes;
- added directories can extend `workspace` scope without selecting `machine`;
- protected paths hide named trees under either scope.

Built-in tools enforce these paths inside Skot. Model-owned processes receive
an operating-system filesystem boundary whenever the selected policy needs
one. Neither layer filters network egress or makes hostile code safe to run.
Use a dedicated container or virtual machine when the model or the code it runs
is untrusted. User-owned `!` and `!!` commands intentionally use the user's
ordinary environment and filesystem permissions.

### Filesystem scopes

- `workspace` (default) limits built-in file tools and model-owned processes to
  the workspace and any added directories. Processes also receive required
  runtime files and a disposable per-workspace tool home;
- `machine` lets explicit built-in file paths and model-owned processes reach
  the surrounding filesystem, minus any explicit protected paths. Inside a
  container, “machine” means that container's filesystem boundary, not the host
  filesystem.

Content exposed to model-owned tools may be included in requests to the selected
model provider. `read-only` prevents writes and command execution; it is not a
confidentiality boundary for readable data.

An absolute path inside the workspace or an added directory is valid in
`workspace` scope. Relative paths always start at the workspace, so
`../sibling` needs `machine` scope unless that sibling was added explicitly.
Changing the filesystem scope, added directories, or protected paths affects
new file-tool calls and process launches; calls and processes already running
are unchanged.

`workspace` uses a minimal environment, sets `HOME` to the disposable tool
home, and keeps `TMPDIR` inside it. `machine` preserves the ambient environment
and ordinary `HOME`. In both scopes Skot removes its own settings and known
provider-key variables from model-owned process environments. This filtering
reduces accidental disclosure through output and logs; it is not a credential
boundary.

Process launches in `workspace` always require a platform filesystem backend.
In `machine`, they require one only when protected paths are configured.
Built-in file tools enforce their path policy independently. Skot verifies any
required process boundary before use and stops rather than silently widening
access if it is unavailable or ineffective. On Linux, installed boundaries
require Landlock ABI V3; macOS uses Seatbelt.

### Added directories

Use repeatable `-add-dir` flags to make directories outside the workspace
available under `workspace` scope:

```sh
sk -add-dir ../shared -add-dir /tmp/generated "update the project"
```

An added directory must already exist. Absolute paths are used directly, `~/`
is relative to the user's real home, and other paths are relative to the
workspace root. A protected path always wins where it overlaps an added
directory.

Directories passed with `-add-dir` last for one run. Directories added through
`/scope` are remembered for that workspace.

### Protected paths

`protected_paths` is empty by default. The Skot data directory is not protected
automatically: add `~/.skot` explicitly if it should be hidden, just like any
other sensitive directory. Values in `config.json` apply to every run using that
data directory, repeatable `-protect-path` flags protect for one run, and paths
protected through `/scope` are remembered for that workspace.

```json
{
  "protected_paths": ["~/.skot", "~/.ssh", "~/.aws", ".env"]
}
```

Absolute paths are used directly, `~/` is relative to the user's real home,
and other paths are relative to the workspace root.

Protection applies in either scope to built-in file tools, model-owned Bash,
and program tools. A configured executable or workdir cannot be inside a
protected tree. Symlink aliases do not bypass protection, and protected entries
are omitted from listings and searches.

On Linux, if a protected path is inside an otherwise writable directory, Bash
and program tools cannot list that directory or create entries directly in it.
Built-in file tools do not have this limitation.

## Scripts and unattended runs

A prompt is read from arguments, or from stdin when no prompt argument is
present:

```sh
sk "review this change"
git diff | SK_TOOLS=read-only sk "review this patch"
```

The ordinary answer is written to stdout. Progress, diagnostics, resume hints,
and verbose events go to stderr. `-retry-budget`,
`-stream-idle-timeout`, and `-max-tool-iterations` bound unattended
behavior.

### JSON output

`-json` writes exactly one versioned product-run object to stdout. A run can
span retries, tools, compaction, and multiple provider responses, so this is not
a raw provider response.

```json
{
  "version": 1,
  "reply": "The tests pass.",
  "usage": {
    "input_tokens": 1200,
    "cached_input_tokens": 800,
    "output_tokens": 90,
    "reasoning_tokens": 40,
    "total_tokens": 1290
  },
  "status": "completed",
  "duration_ms": 2450,
  "model": "deepseek/deepseek-v4-flash",
  "reasoning_effort": "high",
  "tool_set": "read-only",
  "model_attempts": 1,
  "run_id": "run_...",
  "session_id": "session_..."
}
```

The stable fields are `version`, `reply`, `usage`, `status`,
`duration_ms`, `model`, `reasoning_effort`, `tool_set`, and
`model_attempts`. Depending on lifecycle and outcome, the object may also
contain `run_id`, `session_id`, `tool_limit_reached`, `detached_jobs`,
and `error`.

`usage.reasoning_tokens` is a reported subset of
`usage.output_tokens`, not an additional token count. Providers that do not
publish the breakdown leave it at zero.

### Exit codes

| Code | Meaning |
| ---: | --- |
| `0` | Success. |
| `1` | Other application or runtime failure. |
| `2` | Invalid/configuration request, incomplete model result, or fatal tool failure. Inspect or change the invocation before rerunning. |
| `3` | Provider failure. The unchanged invocation may be retryable depending on the diagnostic. |
| `130` | Interrupted or cancelled. |
