# Variables and secrets

Variables configure an app without changing its code. They are set on the
server and override the `[env]` of `moorage.toml`.

```bash
moor env set LOG_LEVEL=debug FEATURE_X=on
```

Every change is a new release of the app's current build, rolled out like a
deploy: new instances start with the new variables, take the traffic once
healthy, and the old ones stop. Nothing is dropped. `--no-restart` keeps the
change for the next deploy instead. Variables can be set before the first
deploy.

```bash
moor env
```

lists them with where each comes from (this environment, every environment,
or `moorage.toml`) and which ones another source overrides.

## Secrets

```bash
moor env set --secret DATABASE_URL
```

asks for the value without showing it (or reads it from standard input:
`moor env set --secret DATABASE_URL < url.txt`), so it stays out of your
shell's history. A secret is stored encrypted, listed without its value, handed to the app
without being written to disk, and replaced by `[secret]` where the app, its build or its
release command prints it (for values of six characters or more). A maintainer can read one
back with `moor env reveal NAME`, which is recorded in the [audit log](accounts.md#who-did-what).

The key that encrypts secrets, `keys/master.key` in the data directory, must
be backed up apart from the database. A server whose secrets were encrypted
with another key, or without its key, refuses to start.

## What an app expects

An app can say what it needs, with no values in the repository:

```toml
[vars.PAYMENTS_KEY]
required = true
secret = true
description = "Sandbox key for the payments provider"

[vars.SESSION_KEY]
generate = "secret"
```

A deployment that lacks one that is `required` fails early, saying which, before the app gets as far as falling
over, and one that is `secret` must have been set as one. One to `generate` is made the first time it is needed, once for each
environment, so that every preview gets its own. Rollbacks do not stop to check. `moor env` and the app's
**Variables** page list them all, with their descriptions, and say what is missing.

## In other environments

A variable belongs to one environment, or with `--all-envs` to all of them. [A preview](previews.md)
also inherits production's, and shows them as inherited; setting one of the same name on the preview
overrides it. Keep one from previews with `--production-only`:

```bash
moor env set --secret --production-only STRIPE_KEY
```

## More

- `--all-envs` sets a variable for every environment of the app; an
  environment's own variable of the same name wins.
- `--build` makes a variable available to builds too (a private package
  index token, for instance); it reaches the app with its next build.
- `moor env unset NAME…` removes variables.
- `moor env import .env` sets the variables of a `.env` file in one change
  (with `--secret`, all as secrets); `moor env export` writes the variables
  as a `.env` file, secrets as comments.
- `moor rollback` keeps the current variables; `moor rollback --with-config`
  restores the ones the earlier release ran with.
- `PORT`, `HOST` and the `MOORAGE_*` variables are set by Moorage and cannot
  be changed.
