# The API

Everything the command line does goes through the REST API, at `/api/v1` on the
console's hostname. [`openapi.json`](openapi.json) is its OpenAPI 3.1 document,
also served by every server at `/api/v1/openapi.json`: every endpoint, its
permission (`x-moorage-permission`), and its documents.

## Authentication

Send an API token ([Accounts and access](accounts.md#api-tokens)) as a bearer
token. Tokens are never accepted in a query string.

```bash
curl -H "Authorization: Bearer $MOOR_TOKEN" https://moorage.apps.example.com/api/v1/apps
```

A browser's session cookie works too. A cookie-authenticated request that
changes anything must also send the session's CSRF token in `X-CSRF-Token`, and
come from the console's own origin.

## Lists

Lists that can grow come in pages, up to `limit` items (at most 200). Pass
`next_cursor` back as `cursor` for the next page; there is none on the last.

```bash
curl -H "Authorization: Bearer $MOOR_TOKEN" "https://moorage.apps.example.com/api/v1/apps?limit=2"
```

```json
{
  "items": [
    { "name": "docs", "url": "https://docs.apps.example.com", "created_at": "2026-11-03T15:42:48Z" },
    { "name": "hello", "url": "https://hello.apps.example.com", "created_at": "2026-11-03T16:01:12Z" }
  ],
  "next_cursor": "aGVsbG8"
}
```

## Running an app

`GET /api/v1/apps/{app}/envs/{env}` says how an environment stands: its release, how many
instances of each process it runs, and those instances. `PATCH` scales it, and answers
the same, once it has got there; a new instance that cannot start leaves it as it was,
with the code `scale_failed`:

```bash
curl -X PATCH -H "Authorization: Bearer $MOOR_TOKEN" -d '{"formation": {"web": 3}}' \
  https://moorage.apps.example.com/api/v1/apps/shop/envs/production
```

`POST …/redeploy` builds what the current release was built from again, and answers with
the queued deployment, as a deploy does. `GET …/releases/{version}` has a release with its
resolved manifest, and `GET …/releases/{version}/diff/{other}` what changed between two:
of variables, their names only.

## Projects

`GET /api/v1/projects` lists those you have a role in, and `/projects/{project}` has one
with its apps and members. An app that you have no role with answers 404, as if it did not
exist; one that your role does not allow answers 403, and says which role would. `PUT` and
`DELETE /projects/{project}/members/{user}` change who is what, by email address or ID.
`POST /apps` makes an app in a project, `GET /apps/{app}` says how much room it takes,
`PATCH /apps/{app}` moves or renames it, and `DELETE /apps/{app}` removes it, with
`?keep_data=true` to keep its data directory. `POST
…/vars/{name}/reveal` reads a secret, and is recorded, as is everything at `GET /audit`.

## Logs

`GET /api/v1/apps/{app}/envs` lists its environments, and `GET /api/v1/previews` the previews of
every app you can see, with when each expires. `POST` to `/envs` makes one, and to `/previews` one of a branch;
`POST /envs/{env}/promote`, `/lock` and `/unlock` do what they say, and `PATCH` with `pinned` keeps a preview.

`GET` and `PUT /envs/{env}/access` read and change [who can reach it](access.md), and `/envs/{env}/share-links`
makes, lists and with `DELETE` revokes links.

`GET /api/v1/apps/{app}/envs/{env}/logs` streams an environment's logs as NDJSON, newest last:
`tail` lines, then with `follow=1` new ones as they come. `source` chooses from `stdout`,
`stderr`, `moorage`, `build`, `access` and `events`; `process`, `instance`, `grep`,
`problems`, `since` and `until` narrow them down. Each line has its `source`, and a `level`
and `message` if it says. `format=text` sends plain text, and `download=1` a file of up to
200,000 lines. A follower that reconnects says where it got to with `after`.

## What happened

`GET /api/v1/events` lists what happened on the server, newest first, in pages, for an
`app` and an `env` if given. `GET /api/v1/events/stream` follows it as server-sent
events, each with the event's ID and, as its data, the same document. A reconnecting
client says where it got to with the `Last-Event-ID` header, or `?after=`, and is sent
what it missed. A token restricted to an app sees only that app's.

Events of a `deployment.started` kind are `transient`: they are for those following now,
and are not kept.

## Errors

Errors are RFC 9457 `application/problem+json` documents. `code` is stable, for
programs; `detail` is for people; `errors` says what is wrong field by field.

```json
{
  "title": "Unprocessable Entity",
  "status": 422,
  "code": "invalid_field",
  "detail": "A token's scopes are read, deploy, admin and gate.",
  "errors": [{ "field": "scopes", "detail": "A token's scopes are read, deploy, admin and gate." }]
}
```

`401` means the credentials are missing or wrong, and `403` that they do not
allow this. `429` comes with a `Retry-After` header.

## Stability

Changes within `v1` only add. A breaking change would be `v2`.
