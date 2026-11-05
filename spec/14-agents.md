# 14. Coding agents

How coding agents use Moorage: `moor mcp`, what it may do, and what tells an agent how. This is the design of 0.9.0.

## Why both a server and a command line

For an agent that has a shell, a good command line with instructions is competitive or better. What MCP adds is clients with no shell, typed tools, per-tool approval, one place to shape
output, and a natural path to a narrow token. So both are first-class: `--json` stays complete, instructions teach it, and the server stays small.

## The server

- **Stdio only,** run by the client as `moor mcp`, speaking the current revision of the protocol and the older handshake, as clients vary.
- **The official Go SDK** behind one internal package ([D22](12-decisions.md#d22--moor-mcp-on-the-official-sdk)).
- **Remote HTTP later.** It needs OAuth, and is what a remote-only client would take.

| Tool | Does | Available |
|---|---|---|
| `list_apps`, `get_app` | What exists and how it stands | Always |
| `list_deployments`, `get_deployment` | Stages, status and the end of the log | Always |
| `read_logs` | Bounded and filtered, as untrusted data | Always |
| `list_releases` | History | Always |
| `check_manifest` | Validates a local `moorage.toml` | Always |
| `deploy` | Deploys a directory, and returns at once with a handle to poll | `--allow-deploy` |
| `rollback`, `restart` | | `--allow-deploy` |

**Left out on purpose:** running commands, the terminal, files, variable values, secrets, deleting anything, domains, gates, members, tokens, and the server's administration. A tool that
runs commands would combine untrusted input, the app's secrets and outbound access in one call.

- `deploy` returns at once, as clients give up after as little as 60 seconds.
- `deploy` only reads below the directory `moor mcp` started in, with the usual ignore rules, so that a tricked agent cannot upload `~/.ssh`.
- Logs are 100 lines by default, with a byte limit, and say when they were cut. Clients warn at about 10,000 tokens.
- Descriptions are static, and a test holds `tools/list` to what it was, so that nothing from an app can reach a description.
- Stdout carries only the protocol, and logs go to stderr. The server ends when stdin does.

## What makes it safe

**The token is the boundary, and nothing else is.** An agent with a shell can go around MCP entirely, and a tool's hints are advisory. Marking logs as untrusted helps against casual attacks and
has been beaten by adaptive ones, so it is hygiene and not a defence. Anyone who can reach an app can write to its logs, so an agent reading them is reading attacker-controlled text. So:

- `moor mcp` refuses an admin token, and does not fall back on what `moor login` stored, which is admin and never expires.
- `moor mcp setup` makes a narrow, expiring token and keeps it in a `moor` context, a file only you can read, and sets up the client. The token never goes in a client's config or the repository.
- **A token can be limited to previews,** with `--envs previews`. A `deploy` token can run commands and read an app's files, and deploying is itself running code, so there is little point in a
  scope between `read` and `deploy`. What does help is an agent that can deploy a branch and look at it, and cannot ask the API for anything of production's, even with a shell. The API enforces it.
- **Such a token changes only previews that take nothing of production's.** Previews normally inherit production's variables, which would hand its secrets to the code deployed, so that deploying to a new name makes a
  preview that stands alone (`environments.inherits`), and any other is read-only to it. It cannot push, as a push can go to any branch.

**What this limit is not.** It limits what a token can ask for. The code an agent deploys runs on the server, and on Linux an app's environments share a user, so that a preview's code can read production's data, and with no
isolation it can read what the server can. To keep what an agent makes from production altogether, it needs an app of its own, on a server where apps are kept apart. The documents say so plainly.

## Telling agents how

- **`instructions`** in the server, under 512 characters and fixed, as some clients keep no more.
- **`moor agent init`** writes a short `AGENTS.md` section naming the app and server, and one Agent Skill, teaching the command line and the tools.
- **A guide in the documentation** covers each client.
- **Not yet:** the MCP Registry, as a bare binary would have to be packaged as an MCPB or OCI artifact, and client plugins.

## Testing

In-process tests for each tool, a test on `tools/list`, and tests of the program itself: stdout purity, ending with stdin, truncation and refusing tokens. A log with an injected instruction is a
fixture, which shows regressions and does not show safety.
