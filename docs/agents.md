# Coding agents

Coding agents can deploy to Moorage and read how it went, through `moor mcp` or the command line. This page says how to connect one, what it can do, and what limits it.

```bash
moor mcp setup --client claude-code --app shop
```

That makes a token that can only read, keeps it in a context of its own, and tells Claude Code how to run `moor mcp`.

## Connect a client

`moor mcp setup` makes the token, and takes `--client` with any of the names here. Where a client has a command of its own, it runs that. Otherwise it says what to put where. With no `--client`, it lists them all, with the whole path to `moor`, as a desktop app does not start from your shell.

| Client | `--client` | It runs, or says to put | Its documentation |
|---|---|---|---|
| Claude Code | `claude-code` | `claude mcp add --transport stdio --scope user moorage -- moor mcp` | [MCP](https://code.claude.com/docs/en/mcp) |
| Codex CLI | `codex` | `codex mcp add moorage -- moor mcp` | [MCP](https://learn.chatgpt.com/docs/extend/mcp?surface=cli) |
| VS Code | `vscode` | `code --add-mcp` with the server as JSON | [MCP](https://code.visualstudio.com/docs/copilot/reference/mcp-configuration) |
| Gemini CLI | `gemini` | `gemini mcp add -s user moorage moor mcp` | [MCP](https://geminicli.com/docs/tools/mcp-server/) |
| Cline | `cline` | `cline mcp add moorage --yes -- moor mcp` | [MCP](https://docs.cline.bot/mcp/configuring-mcp-servers) |
| Cursor | `cursor` | `~/.cursor/mcp.json` | [MCP](https://cursor.com/docs/context/mcp) |
| Antigravity CLI | `antigravity` | `~/.gemini/config/mcp_config.json` | [MCP](https://antigravity.google/docs/mcp/) |
| Devin Desktop, formerly Windsurf | `devin` | `~/.config/devin/mcp_config.json` | [MCP](https://docs.devin.ai/desktop/cascade/mcp) |
| Junie | `junie` | `~/.junie/mcp/mcp.json` | [MCP](https://junie.jetbrains.com/docs/junie-cli-mcp-configuration.html) |
| JetBrains AI Assistant | `jetbrains` | Settings, Tools, AI Assistant, Model Context Protocol (MCP) | [MCP](https://www.jetbrains.com/help/ai-assistant/mcp.html) |
| Zed | `zed` | Its settings, under `context_servers` | [MCP](https://zed.dev/docs/ai/mcp) |
| Goose | `goose` | `~/.config/goose/config.yaml` | [Extensions](https://goose-docs.ai/docs/getting-started/using-extensions) |
| Claude Desktop | `claude-desktop` | `claude_desktop_config.json` | [MCP](https://modelcontextprotocol.io/docs/develop/connect-local-servers) |

These were each read from the client's own documentation in November 2026, and these tools change quickly, so that their pages have the last word. Clients that take only a remote server, such as ChatGPT, cannot use it yet, as `moor mcp` runs on your own computer.

The token goes in `moor`'s own file, which only you can read, and **in no client's configuration or repository.** If a client runs on another computer or in a container, give it `$MOOR_SERVER` and `$MOOR_TOKEN` instead, with a token from [`moor tokens create`](accounts.md#api-tokens).

## What it can do

| Tool | Does | Available |
|---|---|---|
| `list_apps`, `get_app` | What exists and how it stands | Always |
| `list_deployments`, `get_deployment` | Stages, status and the end of the log | Always |
| `read_logs` | The newest lines, filtered if asked | Always |
| `list_releases` | What has run, and what can be gone back to | Always |
| `check_manifest` | Checks a local `moorage.toml`, and says how Moorage would build and run the app | Always |
| `deploy` | Uploads a directory, and returns at once with an id to ask about | `--allow-deploy` |
| `rollback`, `restart` | | `--allow-deploy` |

**Nothing here runs a command, reads a file or a secret, or deletes anything,** and nothing manages domains, gates, members, tokens or the server. That is on purpose: a tool that runs commands would put what an app has written, its secrets and the way out all in one call.

`check_manifest` and `deploy` read only below the directory that `moor mcp` started in, or `--root DIR`, with the same rules as `moor deploy` for what to leave out. They refuse if that directory is your home directory or above it, a directory that is hidden or ignored, and a tree with a link that leads out of it. If a client starts it somewhere else, say where to read from:

```bash
moor mcp --root ~/code/shop --app shop
```

## Letting it deploy

```bash
moor mcp setup --client claude-code --app shop-dev --deploy
```

With `--deploy`, a token can deploy, roll back and restart, and only in previews, unless you say `--envs all`. Such a token:

- Deploys only to a name that starts with `agent-`, such as `agent-new-pricing`, which makes a preview of that name. The app has to exist already: create it yourself, with `moor apps create`.
- **Can reach only those previews, for reading as well as changing.** They take nothing of production's variables or those set for every environment, as the code it deploys could otherwise read them. A required variable has to be set on the preview by someone who can, with `moor env set`.
- Is refused anything else, including what is added to Moorage later. It cannot read production's logs, variables, files, deployments or runs, promote, change variables, lock or pin an environment, or read the app's events.
- Cannot use git, either to push or to clone, as a repository holds every branch.
- Is held to what any preview is: at most 10 to an app, and it expires after [the usual time](previews.md). What the code it deploys says in its manifest does not change that, or how many of production's releases are kept.

## What this does not do

**The token is the boundary, and nothing else here is.** An agent with a shell can go around `moor mcp` altogether, and a tool's own description of itself is only advice to a client. So:

- **Limiting a token limits what it can ask for, and code it deploys runs on your server.** On Linux, [an app's environments share a user](isolation.md#with-a-user-of-its-own), so that code in a preview can read production's data directory, and with no isolation it can read what the server can. **To keep what an agent makes from production altogether, give it an app of its own, on a server where apps are kept apart,** with `--app`.
- **Logs are written by the app and whoever can reach it.** An agent that reads them is reading what a stranger may have written, and could be told by it to do something. Moorage marks them, and removes what is hidden from view, which stops careless attempts and not determined ones. So keep what an agent can do small, and treat what it did after reading logs with care.
- **A client's approval prompts help,** and people approve nearly all of them, so that they are not the protection either.

`moor mcp` refuses a token that can do everything, and does not fall back on what `moor login` stored, which never expires.

## Telling an agent how

An agent does better knowing what Moorage is, and what to be careful of:

```bash
moor agent init --app shop
```

That writes a section of `AGENTS.md`, and an [Agent Skill](https://agentskills.io/home) in `.agents/skills` and `.claude/skills`, which between them most clients read. They teach `moor` and the tools, and hold no token, so that you can commit them. Running it again brings them up to date, and leaves the rest of `AGENTS.md` as it was.

Claude Code reads `AGENTS.md` only where there is no `CLAUDE.md`. It finds the skill either way, or you can add `@AGENTS.md` to `CLAUDE.md`.

## With no MCP at all

An agent with a shell can use `moor` itself, which is often just as good. Lists take `--json`, and the skill teaches it. Give it a narrow token the same way:

```bash
moor mcp setup --app shop --deploy
```

```bash
moor context use agent
```

## See also

- [API tokens](accounts.md#api-tokens)
- [Previews and environments](previews.md)
- [How apps are kept apart](isolation.md)
