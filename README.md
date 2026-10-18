# Skot

Skot is a small, opinionated agent for the terminal. It has a deliberately
small set of tools and supports both interactive sessions and one-shot runs.

The built-in tools and defaults give you a complete setup out of the box.
Plans, roles, and larger workflows are left to prompts. Sessions and job state
stay local.

It ships as a single Go binary with no runtime to install alongside it, starts
in a few milliseconds, and stays light on memory. The same binary serves an
interactive terminal and a shell pipeline; neither is a second-class mode.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/levmv/skot/main/install.sh | sh
```

Update an installed release in place. Running processes keep the old version
until they are restarted:

```sh
sk update
```

Or build from a checkout:

```sh
cd skot
make build
```

## Quick start

Start an interactive session and authenticate with `/login`:

```sh
sk
```

Or supply a key through the environment and run a single prompt:

```sh
export DEEPSEEK_API_KEY=...
sk "inspect the workspace and run the tests"
```

Skot works with DeepSeek, Anthropic, Google Gemini, OpenAI, OpenRouter,
OpenCode Go, and Ollama. Models are named `provider/model`; `/model` lists and
switches them mid-session.

Data lives in `~/.skot` by default. Set `SK_HOME` or pass `-home` to move it.

## Sessions and jobs

Running `sk` without a prompt starts a saved session. One-shot runs are
normally discarded when they finish. Use `-save-session` to keep one:

```sh
sk -save-session "fix the failing tests"
sk resume                         # latest session for this workspace
sk resume 0f3a "continue the fix"   # ID or unambiguous prefix
```

A Bash command still running after about ten seconds becomes a managed job the
model can inspect, wait for, or stop. Background jobs can outlive the Skot
process and be adopted when the session is resumed.

## Scripts

Pass a prompt as arguments or through stdin. When arguments are present, stdin
is ignored. To review a diff, send both the instruction and the diff through
stdin. Answers go to stdout; diagnostics go to stderr:

```sh
{
  printf 'Review this patch:\n\n'
  git diff
} | SK_TOOLS=read-only sk
SK_TOOLS=read-only sk -json "summarize this project" > result.json
```

`-json` returns one JSON object with the answer, token usage, and completion
status. Retry and tool-call limits help keep unattended runs from continuing
indefinitely. The [reference](docs/reference.md#scripts-and-unattended-runs)
describes the JSON fields and exit codes.

Runs from scripts use flags and environment variables, ignoring preferences
saved in the interactive UI. Resuming a session can also restore its model and
reasoning effort.

## Tools

A tool set lists the tools the model can use. For example, `read-only` allows
reading and searching, and `none` disables tools. You can also define custom
tools that run a command with JSON input.

Delegation is optional. When the `agent` tool is enabled, child agents share the
current workspace and filesystem scope but receive only built-in read-only
tools; they cannot edit files or create more agents.

## Filesystem access

Tools run without asking for confirmation. The default `workspace` scope gives
the model access to the project, plus the runtime files needed to execute
commands. Use `-add-dir` to allow another directory or `-scope machine` to allow
the surrounding filesystem. Protected paths exclude selected locations in
either scope.

This does not filter network access or make hostile code safe to run. Use a
dedicated container or virtual machine for that threat model. See
[filesystem details](docs/reference.md#filesystem-access).

## Documentation

See the [complete user reference](docs/reference.md). Run `sk -help` for CLI
syntax, type `/` in the interactive UI for commands, or use `/help` for keyboard
shortcuts.

## Go packages

Skot's reusable packages live under `github.com/levmv/skot`.

| Package | Purpose |
| --- | --- |
| [app](app/) | The Skot application: sessions, settings, model selection, credentials, and tools, ready for a frontend. |
| [agent](agent/) | The model and tool execution loop, with a persistent journal, retries, context management, and compaction. |
| [tools](tools/) | Reusable workspace, process, and web tools, plus filesystem access policies and process management. |
| [provider](provider/) | Model configuration and authentication, exposing both a portable backend and a raw protocol connection. |
| [model](model/) | Shared requests, responses, conversation items, tool definitions, usage, errors, and conversation replay across models. |
| [model/transport](model/transport/) | HTTP requests, authorization, and server-sent event streams for callers that control the protocol body. |
| [model/chatcompletions](model/chatcompletions/) | Chat Completions backend and an observer for raw responses. |
| [model/responses](model/responses/) | OpenAI Responses backend. |
| [model/anthropic](model/anthropic/) | Anthropic Messages backend. |
| [model/google](model/google/) | Gemini GenerateContent backend and an observer for raw responses. |
| [web](web/) | Standalone web search and page fetching through supported services or direct HTTP fetching. |
| [imageinput](imageinput/) | Image preparation for model input: decoding, orientation, resizing, and metadata removal. |
| [images](images/) | Standalone image generation and editing through the OpenAI and OpenRouter image APIs, with usage and costs. |

For an application with its own conversation storage and execution loop, start
with `provider` and `model`. `provider.Open` uses Skot's model catalog and provider
defaults; `provider.New` accepts explicit configuration. Each backend call makes
one generation attempt; the caller manages retries, tool execution, and history.
Use `agent` for Skot's execution loop with your own backend, journal, and tools,
or `app` for the assembled application with Skot's settings and session handling.

Examples: [model calls](provider/example_test.go) and
[raw protocol requests](provider/raw_example_test.go).

## Credits

Special thanks to [Valerii Ishchenko](https://github.com/valeriyischenko) for
his substantial contributions to the project's design and implementation.

Skot is distributed under the repository's [MIT License](LICENSE).
