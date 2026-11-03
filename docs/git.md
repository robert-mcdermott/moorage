# Deploying with git push

Every app has a git repository on the server. Pushing its production
branch deploys it: the build is shown in your terminal as it happens, and
the push fails if the deploy does.

## Once: register your SSH key

```bash
moor keys add
```

That registers `~/.ssh/id_ed25519.pub` (or `id_ecdsa.pub`, or `id_rsa.pub`;
name another file to use it). Moorage accepts Ed25519, ECDSA and RSA keys of
3072 bits or more. `moor keys` lists the keys, `moor keys remove NAME` removes
one. Until accounts arrive, every registered key may push to every app.

## For each app: add the remote, push

In the app's repository:

```bash
moor git remote
```

```bash
git push moorage main
```

`moor git remote` adds a remote named `moorage`, for the app named by
`[app].name` in `moorage.toml` (or `--app`, or the directory's name), such as
`ssh://git@moorage.apps.example.com:2222/hello.git`. The app need not exist:
the first push creates it.

```text
remote: moorage  created the app hello
remote: moorage  hello · production · dep_01M6HGSPPT25AQVD820A37WKEF
remote: ▸ source     main @ 9b8f6c7  First version
remote: ▸ detected   static
remote: ▸ built      9 files, 9.8 KiB in 0s
remote: ✔ live       v1  https://hello.apps.example.com  (0s)
To ssh://moorage.apps.example.com:2222/hello.git
 * [new branch]      main -> main
```

The first time, `ssh` asks you to trust the server's host key: its
fingerprint is in the server's log, on the line that starts the SSH
listener (`git pushes over SSH … host_key=SHA256:…`).

Once the remote is there, the other commands find the app from it:
`moor logs -f`, `moor rollback` and the rest need no `--app`.

## What a push does

| Push | What happens |
|---|---|
| The production branch | Deployed to production. The production branch is the repository's default branch: `main`, or, if the first push had no `main`, the first branch it pushed. |
| Another branch | Kept, not deployed (previews of other branches arrive in 0.4.0). |
| A tag | Kept, not deployed. |
| `git push moorage --delete BRANCH` | The branch is deleted; the production branch cannot be. |
| A commit whose message contains `[skip deploy]` | Kept, not deployed. |

**A failed deploy fails the push.** The deploy runs before the branch moves:
if it fails, git reports the push as rejected, with the deployment's ID and
why, and the branch on the server stays where it was. Fix the problem and
push again. The previous release keeps serving meanwhile.

**`Ctrl-C` leaves the deploy running.** Follow it with
`moor deployments follow ID`; the branch moves if it succeeds.

**Force pushes are accepted**: Moorage is where you deploy, not where the
code is kept. Two pushes to one branch do not deploy at once: the second is
refused until the first has finished. Pushes to different branches, or apps,
go on side by side.

## Push options

`git push -o OPTION` changes one push:

| Option | Effect |
|---|---|
| `skip-deploy` | Keep the commits without deploying them. |
| `detach` | Accept the push at once; the deploy goes on (follow it with `moor deployments follow ID`). |
| `env=production` | Deploy this branch to production, whatever its name. |

```bash
git push -o detach moorage main
```

## Over HTTPS

Where SSH is blocked, push over HTTPS with the API token:

```bash
moor git remote --http
```

The remote is `https://moorage.apps.example.com/git/hello.git`, and
`moor git remote --http` sets up `moor git credential` for it, a git
credential helper that gives git the token of the server `moor` talks to.
The token is not given to any other host, and not stored in a keychain.

## What is deployed

The commit's files, as `moor deploy` would upload them: what `.moorignore`
excludes is left out, and so are `node_modules`, `.venv` and `__pycache__`.
Symbolic links and submodules are left out, and the push says so. Git LFS
is not supported: the app gets the pointer files, and the push warns.

`moor deployments` shows what each push deployed:

```text
ID                               SOURCE              STATUS   RELEASE   TOOK   CREATED
dep_01M6HGT4BCTV2TNH0QKAN2W603   push main@0bb5a15   live     v2        0s     just now
```

## Limits

- A push may be up to 1 GiB.
- `git push --atomic` is not supported, and a failed deploy refuses every ref
  of the push it belongs to.
- The repository can be cloned and fetched (`git clone ssh://…/hello.git`),
  with the same keys and token.
- `listen.ssh` in `server.toml` sets the SSH port (2222 by default); set it
  to `""` to turn SSH pushes off.
