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
