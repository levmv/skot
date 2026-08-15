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

`sk update` downloads the latest `skot-v*` GitHub release for the current
platform, verifies its SHA-256 checksum, and atomically replaces the running
executable. Existing processes continue with the old version until restarted.
Development builds report an error instead of overwriting themselves.

Use `--` when prompt text could otherwise be parsed as a flag:

```sh
sk -- "-review the command-line parser"
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
| `-root path` | `SK_ROOT` | Workspace root for file and process tools. Default: current directory. |
| `-tools name` | `SK_TOOLS` | Select the tool set available to the model. Product default: `default`. |
| `-tools-file path` | `SK_TOOLS_FILE` | Load custom program tool definitions. Default: `tools.json` in the Skot data directory. |
| `-sandbox policy` | `SK_SANDBOX` | Select `auto`, `workspace`, `masked`, or `off`. Default: `auto`. |
| `-home path` | `SK_HOME` | Select the Skot data directory. Default: `~/.skot`. |
| `-journal path` | — | Use an explicit JSONL session journal. |
| `-save-session` | — | Retain a one-shot invocation as a resumable managed session. |
| `-json` | — | Emit one versioned JSON result on stdout. |
| `-v` | — | Show attempts, retries, tool activity, maintenance, status, and final token usage on stderr. |
| `-version` | — | Print the version and exit. |

`SK_COLOR=always|never` overrides automatic styled-output detection.
`NO_COLOR` disables styling.

For the model, reasoning effort, tool set, and sandbox, an explicit flag or
environment variable wins over the persisted interactive selection. When a
session is resumed without an explicit model, its recorded model and effort are
restored.

## Models and credentials

Models are addressed as `provider/model`. Use `/model` interactively to see
the currently available route list and switch models.

| Provider | Credential |
| --- | --- |
| DeepSeek | `DEEPSEEK_API_KEY` |
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
| Tavily | `TAVILY_API_KEY` | Web search |
| Exa | `EXA_API_KEY` | Web search and fetch |
| Firecrawl | `FIRECRAWL_API_KEY` | Web-fetch fallback |

An explicit `-model-api` or `-base-url` can make a route unverified. Skot
retains conservative protocol defaults in that case and reports a compatibility
hint if the provider rejects the request. Route-specific features such as a
thinking switch are not carried across an incompatible protocol override.

## Data directory and configuration

Skot uses `~/.skot` by default. `SK_HOME` and `-home` select another data
directory. It contains:

- `config.json` — settings, custom tool sets, child-model allowlist, and
  protected paths;
- `auth.json` — credentials managed by `/login` and `/logout`;
- `tools.json` — the default custom program tool catalog;
- `sessions/` — managed session journals and child-agent state.

The directory is created with private permissions. Session journals contain
conversation and workspace data and should also be treated as private.

`config.json` is strict JSON: unknown fields and multiple top-level values are
rejected. A representative configuration is:

```json
{
  "model": "deepseek/deepseek-v4-flash",
  "reasoning_effort": "high",
  "tool_set": "delegate",
  "sandbox": "auto",
  "theme": "auto",
  "tool_sets": {
    "delegate": ["read", "grep", "glob", "edit", "write", "bash", "job", "agent"]
  },
  "agent_models": ["deepseek/deepseek-v4-flash"],
  "protected_paths": [".env", "~/.ssh"]
}
```

| Field | Meaning |
| --- | --- |
| `model` | Persisted default model selected by the interactive UI. |
| `reasoning_effort` | Persisted route-specific effort. |
| `recent_models` | UI-managed recent-model list; normally not edited by hand. |
| `tool_set` | Persisted tool set selection. |
| `tool_sets` | Map of tool set names to exact ordered tool-name lists. A custom definition replaces a built-in set with the same name. |
| `agent_models` | Models that the optional `agent` tool may select explicitly. |
| `sandbox` | Persisted sandbox selection. |
| `theme` | Persisted interactive terminal theme: `auto`, `light`, or `dark`. An unrecognized saved value is reset to `auto` at interactive startup. |
| `protected_paths` | Additional paths hidden from model tools and model-owned processes. |

Use `/login` rather than editing `auth.json` directly.

## Sessions and interactive use

Running `sk` with no prompt creates a managed persistent session. A one-shot
run is ephemeral unless `-save-session` or `-journal` is used, it creates a
child agent, or it leaves detached work running.

```sh
sk -save-session "fix the failing tests"
sk resume
sk resume 01k2
sk resume 01k2 "continue the fix"
```

Session selection is scoped to the canonical workspace path. A short ID may be
used when it identifies exactly one session. Bare `resume` chooses the most
recent session for that workspace.

### Journal compatibility

The journal schema version describes the required semantic projection used to
rebuild session state, rather than the exact set of record kinds or JSON fields.
Adding an optional payload field, or an observational record kind in the
reserved `aux/` namespace, does not by itself change the schema version.

An `aux/` record is a semantic leaf. Ignoring it may change only the replayed
last sequence number: it must not affect conversation blocks, compaction or
pruning boundaries, pending work, usage, configuration, or the validity of any
required record. Unknown kinds outside `aux/` fail replay, and every journal
must still begin with `session_started`. A change to required state or replay
invariants requires a new schema version and an explicit migration.

### Interactive commands

| Command | Action |
| --- | --- |
| `/help` | Show keyboard shortcuts. |
| `/clear` | Start a new session. |
| `/resume [id-or-prefix]` | Choose or resume a previous session. |
| `/login [provider]` | Store a provider or service key. |
| `/model [provider/model]` | List or switch models. |
| `/tools [name]` | Show or switch the active tool set. |
| `/sandbox [auto|workspace|masked|off]` | Show or switch filesystem isolation. |
| `/theme [auto|light|dark]` | Show or persist the interactive terminal theme. Default: `auto`, which asks the terminal for its background colour and falls back to `dark` when there is no answer. Set `light` or `dark` explicitly if your terminal filters that query. |
| `/context` | Show the current context budget. |
| `/compact` | Compact older completed conversation blocks. |
| `/logout [provider]` | Remove a stored key. |
| `/exit`, `/quit`, `/q` | Exit Skot. |

Enter sends a message. Shift/Alt+Enter or Ctrl+J inserts a newline. Escape
cancels the active turn, Alt+Up recalls queued input, and Ctrl+C exits.

`! command` runs a shell command and includes its result in the conversation.
`!! command` runs it privately. Both are user-owned commands and therefore use
the user's normal environment and filesystem permissions rather than the model
sandbox.

Skot loads applicable `AGENTS.md` instructions from the workspace root down
to the current directory.

## Tools and tool sets

Built-in file tools operate inside `-root`. Paths and symlinks may not escape
that root; reads and searches are bounded, and writes are atomic.

| Tool set | Tools |
| --- | --- |
| `default` | `read`, `grep`, `glob`, `edit`, `write`, `bash`, `job` |
| `edit` | `read`, `ls`, `grep`, `glob`, `edit`, `write` |
| `read-only` | `read`, `ls`, `grep`, `glob` |

All built-in tool sets include bounded public `web_fetch`. They include
`web_search` when a Tavily or Exa key is available. A custom tool set is an
exact tool list, not a set of additions to a built-in set.

On Linux, `default` also includes `ls` when the protected Skot home is inside
the workspace. This keeps root listing available whenever Landlock is active,
including after a runtime sandbox switch. Explicit custom tool sets remain
exact.

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

The executable receives one JSON argument object on stdin. Stdout becomes the
model-facing result; stderr remains diagnostic output. A configured tool is
visible only when the active tool set names it.

| Field | Meaning |
| --- | --- |
| `name` | Required tool name: starts with a letter and contains only letters, digits, and underscores. |
| `description` | Required model-facing description. |
| `command` | Required executable and fixed arguments. |
| `parameters` | JSON Schema object for stdin arguments. Defaults to `{"type":"object"}`. |
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

`/context` reports the estimated input budget for the actual route projection,
including instructions, tool definitions, rolling summary, history, and queued
input. Provider-owned reasoning that the selected route will not replay is not
charged to the estimate.

When the next request would exceed its input limit, Skot first tries to prune
older tool-result bodies and then compacts an older completed prefix into a
rolling summary. Maintenance never rewrites the active run. `/compact`
requests the same compaction explicitly.

`-context-window` overrides missing or incorrect route metadata.
`-max-tool-iterations` is an emergency fuse for repeated tool cycles; when it
is reached, Skot asks the model for a final answer without offering tools.

## Security model

Skot's boundaries are designed to contain accidental damage, not an agent
deliberately trying to escape. Use a container or virtual machine when the model
or the code it runs is untrusted.

Three rules define the normal boundary:

- a tool set decides which tools the model has;
- built-in file tools always stay inside the workspace root;
- model-owned processes use the selected sandbox policy.

### What the boundary does not cover

- **Network access.** Model-owned processes inherit it; Skot does not filter
  egress.
- **Sandbox escapes.** Races, kernel bugs, and a determined adversary are
  outside the threat model.
- **Explicit bypasses.** `-sandbox off` and user `!`/`!!` commands use the
  user's normal filesystem permissions.
- **Workspace contents.** The agent may change anything inside the root except
  explicitly protected paths.

### Sandbox policies

- `auto` (default) — `workspace` on a host and `masked` in a detected
  container;
- `workspace` — model-owned processes can access the workspace, required
  runtime files, and a disposable synthetic home;
- `masked` — the container remains the main boundary; processes retain its
  filesystem access except for protected paths;
- `off` — processes retain the user's filesystem access and protected paths
  are disabled. It is never selected automatically.

If the selected boundary cannot be installed or fails its
read/truncate/remove probe, startup stops instead of silently falling back to
`off`. The startup header shows the effective policy.

On Linux, `workspace` and `masked` require Landlock ABI V3 so truncation is
part of the boundary. If it is unavailable, the default fails closed.

`workspace` uses a per-workspace synthetic `HOME` and keeps `TMPDIR` inside
it. `masked` and `off` preserve the ordinary `HOME`. Skot removes its
settings and known provider keys from model-owned process environments, but
environment filtering is not a security boundary.

### Protected paths

The Skot data directory is always protected in `workspace` and `masked`. It may
be inside the workspace; on Linux, model-owned processes can then use existing
entries but cannot list or create entries directly in the workspace root.
Built-in file tools do not have this limitation. Additional paths are configured
in `config.json`:

```json
{
  "protected_paths": [".env", "~/.ssh", "/work/shared/credentials"]
}
```

Absolute paths are used directly, `~/` is relative to the user's real home,
and other paths are relative to the workspace root. Nested and duplicate paths
are collapsed. The filesystem root is rejected. `workspace` and `masked`
also reject a protected path that contains the whole workspace; protecting a
child inside the workspace is valid.

Protection applies to `read`, `ls`, `grep`, `glob`, `edit`, `write`,
model-owned Bash, and program tools. Symlink aliases do not bypass it. Protected
entries are omitted by listing and search tools, and protected `AGENTS.md`
files are not added to model instructions.

Selecting `off` disables protected paths, while the built-in file tools still
remain confined to the workspace.

Landlock is allow-list based. On Linux, protecting a child of a writable
directory can prevent model-owned processes from listing that parent or
creating new siblings there. Built-in file tools do not have this limitation.

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
