# Skot

Skot is a small, opinionated coding agent for the terminal. It has
deliberately few tools and supports both interactive sessions and one-shot
runs.

It stays small on purpose: sessions and job state stay local, capabilities are
explicit, and plans, roles, and larger workflows are left to prompts rather
than built in.

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

Skot works with DeepSeek, Anthropic, OpenAI, OpenRouter, OpenCode Go, and
Ollama. Models are named `provider/model`; `/model` lists and switches them
mid-session.

Data lives in `~/.skot` by default. Set `SK_HOME` or pass `-home` to move it.

## Sessions and jobs

Running `sk` without a prompt starts a persistent session. One-shot runs are
ephemeral unless you save them or leave detached work behind.

```sh
sk -save-session "fix the failing tests"
sk resume                         # latest session for this workspace
sk resume 0f3a "continue the fix" # ID or unambiguous prefix
```

A Bash command still running after about ten seconds becomes a managed job the
model can inspect, wait for, or stop. Background jobs can outlive the Skot
process and be adopted when the session is resumed.

## Scripts

Pass a prompt as arguments or through stdin. The answer goes to stdout and
diagnostics go to stderr, so ordinary shell composition works as expected:

```sh
git diff | SK_TOOLS=read-only sk "review this patch"
SK_TOOLS=read-only sk -json "summarize this project" > result.json
```

`-json` emits exactly one versioned result object for the run. Retry and
iteration limits bound unattended execution, and exit codes distinguish an
invalid or incomplete run from a transient provider failure.

Headless runs do not inherit model, tool-set, or filesystem-scope choices made
in the interactive UI. For non-default behavior, pass flags or set environment
variables; resumed sessions may restore their recorded model and reasoning
effort.

## Tools

A tool set is an exact list of capabilities available to the model. Custom
program tools expose narrow commands with JSON input.

Delegation is optional. When the `agent` tool is enabled, child agents share the
current workspace and filesystem scope but receive only built-in read-only
tools; they cannot edit files or create more agents.

## Filesystem access

Skot does not approve individual commands. A tool set decides what the model
can do; the filesystem scope decides where both built-in file tools and
model-owned processes may do it. `workspace` keeps user paths in the project;
`machine` permits explicit reach into the surrounding filesystem. Protected
paths remove named trees from either scope. This does not filter network access
or make hostile code safe to run. Use a dedicated container or virtual machine
for that threat model. See [scope details](docs/reference.md#filesystem-access).

## Documentation

See the [complete user reference](docs/reference.md). Run `sk -help` for CLI
syntax, type `/` in the interactive UI for commands, or use `/help` for keyboard
shortcuts.

## Credits

Special thanks to [Valerii Ishchenko](https://github.com/valeriyischenko) for
his substantial contributions to the project's design and implementation.

Skot is distributed under the repository's [MIT License](LICENSE).
