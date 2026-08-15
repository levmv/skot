# Skot

Skot is a terminal agent for working with local files, tools, and long-running
processes. Use it as a persistent interactive assistant or as a one-shot command
in scripts.

## Highlights

- Terminal-native interactive sessions with streaming Markdown and ordinary
  scrollback.
- Durable conversations and background jobs that survive restarts.
- Bounded file, search, Bash, job, web, and custom program tools.
- Configurable tool sets and filesystem isolation for model-owned processes.
- Read-only child agents for parallel independent work.
- Supports DeepSeek, OpenAI, OpenRouter, OpenCode Go, and Ollama.

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

Start an interactive session, then use `/login` to authenticate with a provider:

```sh
sk
```

Or provide a key through the environment and run one prompt:

```sh
export DEEPSEEK_API_KEY=...
sk "inspect the workspace and run the tests"
```

Models use `provider/model` names. `/model` lists and switches available models.

## Common workflows

Running `sk` without a prompt starts a persistent session. One-shot runs are
ephemeral unless they are explicitly saved or leave detached work running.

```sh
sk -save-session "fix the failing tests"
sk resume                         # latest session for this workspace
sk resume 01k2 "continue the fix" # ID or unambiguous prefix
```

For scripts, pass a prompt as arguments or through stdin. The normal answer goes
to stdout; `-json` emits one versioned result object.

```sh
SK_TOOLS=read-only sk -json "summarize this project" > result.json
```

Skot uses `~/.skot` as its default data directory. Set `SK_HOME` or pass
`-home` to use another directory.

## Safety

Skot uses workspace boundaries and a filesystem sandbox to contain accidental
damage. These safeguards are not designed to contain an agent deliberately
trying to escape. For that threat model, run Skot inside a dedicated container
or virtual machine. See the [security model](docs/reference.md#security-model)
for details.

## Documentation

See the [complete user reference](docs/reference.md).

Run `sk -help` for CLI syntax or `/help` inside the interactive UI for
commands and keyboard shortcuts.

## Credits

Special thanks to [Valerii Ishchenko](https://github.com/valeriyischenko) for
his substantial contributions to the project's design and implementation.

Skot is distributed under the repository's [MIT License](LICENSE).
