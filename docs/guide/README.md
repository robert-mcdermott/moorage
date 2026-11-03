# Moorage guide

How to install Moorage and deploy apps with it. The design documents in the
[parent directory](../README.md) explain how Moorage works and where it is
going.

1. [Installing Moorage](install.md): get `moor`, configure the server, run
   it as a service, back it up.
2. Deploying, by kind of app:
   - [Go](go.md)
   - [Python](python.md) (with uv)
   - [Node.js](node.md) (with npm)
   - [Static sites](static.md)
   - [Anything else](other.md): your own build, a script, an artifact
3. [Variables and secrets](configuration.md)

Every app is driven the same way once deployed:

| Command | What it does |
|---|---|
| `moor apps` | Lists the apps |
| `moor deploy [DIR]` | Builds and deploys the source in DIR |
| `moor deployments` | Lists an app's deployments; `show` and `follow` one |
| `moor releases` | Lists an app's releases |
| `moor rollback [vN]` | Serves an earlier release again, in seconds |
| `moor ps` | Lists the app's instances: status, health, port, PID |
| `moor logs [-f]` | Shows the app's output |
| `moor restart` | Replaces the instances without downtime |
| `moor env` | Lists and changes variables and secrets |
| `moor manifest explain` | Shows how Moorage will build and run the app |

Each command has `--help`. An app is chosen with `--app`, `$MOOR_APP` or
`[app].name` in the `moorage.toml` of the current directory.
