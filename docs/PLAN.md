---
PLAN: "feat: idempotency — Idempotency-Key middleware that runs a retried request once and replays its stored response"
TAG: v0.1.0
EXECUTOR: jules
REVIEWER: none
---

> This plan is dispatched via the CodeJob workflow. See skill: agents-workflow.

# Plan — `idempotency`: a retried request runs once

## 0. Context (read first)

This repository is new and empty (created with `gonew`; module `webtyp.com/idempotency`).

Offline-first browsers queue the operations they ran while the server was unreachable and send
them when it is back. A send can succeed on the server while the answer is lost (network cut, server
restart), so the client **sends it again**. Each queued operation carries a unique key in the
request header `Idempotency-Key` (constant `router.HeaderIdempotencyKey` in
`webtyp.com/router` **v0.4.1**). This library is the server side: a `router.Middleware` that
remembers "key → response" and, when the same key comes back, **answers with the stored response
without running the handler again**.

It is useful for any retried non-GET request (a double click on Save, a webhook delivered twice),
not only for sync — that is why it is its own repository and not part of `webtyp/rpc` (stateless HTTP
binding) or `webtyp/dbsync` (sync engine).

Relevant contracts (`webtyp.com/router` v0.4.1; read them in the module cache after `go get`):

```go
type HandlerFunc func(Context)
type Middleware func(HandlerFunc) HandlerFunc
// Context: Method() string; Path() string; Body() []byte; GetHeader(k) string; SetHeader(k, v);
//          WriteStatus(int); Write([]byte) (int, error); UserID() string;
//          Decode(model.Decodable) error; Encode(model.Encodable) error; (+ cookies, values, params)
// Router.Use(m ...Middleware)
const HeaderIdempotencyKey = "Idempotency-Key"
```

Ordering fact verified in `webtyp.com/server/httpd` (`adapter.go`, `applySecurityAndMiddleware`):
middlewares added with `Router.Use` run **behind** authentication and the RBAC gate, so when this
middleware runs `ctx.UserID()` is already set and the request is already authorized. `httpd`
captures the middleware list **when each route is registered**: `Use` must be called before the
routes are mounted (say so in the README).

## Design gate

### 1. Prior art
- **IETF draft-ietf-httpapi-idempotency-key-header**: the header, "same key + same request →
  same response", **409** while the first request is still in progress, **422** when a key is
  reused for a different request. Our behaviour.
- **Stripe idempotency keys**: keys stored for 24 h, scoped per account, the stored response
  (status + body) replayed verbatim. Our storage model and TTL.
- **AWS Lambda Powertools Idempotency**: a separate utility with an "in progress" record and an
  expiry, so a crashed request does not block retries forever. Our stale in-progress rule.

### 2. Novice-name test
`idempotency.New(db, time.Now)` → "an idempotency store over this database".
`store.Middleware()` → "the middleware to put on the router". `store.Purge(before)` → "delete
answers older than this".

### 3. Complexity ledger
```
Concepts the developer must learn   +1 (the store and its middleware)
Files they must touch to do X        +1 line at the composition root (Use) + migration
Lines at the call site               +3
Ways to do the same thing            0
```

### 4. Where it belongs
One concern — remember and replay responses by key — in its own repository (it needs a table, so it
cannot live in the stateless `rpc`; it is not sync-specific, so not in `dbsync`). The header name is
owned by `router` so the client (`rpc`) and this server side share it without importing each other.

### 5. What this deletes
Nothing: new capability.

## 1. Model (`model.go`, generated with `ormc`)

```go
var RecordModel = model.Definition{
    Name: "idempotency_record",
    Fields: model.Fields{
        {Name: "id", Type: model.Text(), DB: &model.FieldDB{PK: true}}, // scope + "\x1f" + key
        {Name: "method", Type: model.Text(), NotNull: true},
        {Name: "path", Type: model.Text(), NotNull: true},
        {Name: "state", Type: model.Text(), NotNull: true},   // "in_progress" | "done"
        {Name: "status", Type: model.Int(), NotNull: true},   // stored response status (done)
        {Name: "body", Type: model.Blob()},                   // stored response body (done)
        {Name: "content_type", Type: model.Text()},           // stored Content-Type (done)
        {Name: "created_at", Type: model.Int(), NotNull: true}, // unix ns
    },
}
```
Generate `model_orm.go` with `go install webtyp.com/ormc/cmd/ormc@latest && ormc` at the repo root
and commit it (struct `Record`). Package `webtyp.com/idempotency/migrate`:
`func Migrate(conn ddl.Execer, compiler ddl.Compiler) error` → `ddl.New(conn, compiler).CreateTable(&idempotency.Record{})`
(same convention as <https://github.com/veltylabs/clinical_encounter/blob/main/migrate/migrate.go>).

## 2. Target API (exactly this is exported, plus the generated `Record` helpers)

```go
// Store remembers responses by idempotency key.
type Store struct{ /* unexported */ }

// New returns a store over db (the idempotency_record table must exist; see migrate).
// now returns unix nanoseconds (pass webtyp.com/time.Now).
func New(db *orm.DB, now func() int64) (*Store, error)

// Middleware returns the router.Middleware that applies the rules of §3.
func (s *Store) Middleware() router.Middleware

// Purge deletes "done" records created before the given unix-ns instant; returns how many.
func (s *Store) Purge(before int64) (int, error)

// MsgInProgress is the exact body of the 409 answered while the first request with a key is
// still running; a client that retries treats it as "try later", not as a refusal.
const MsgInProgress = "idempotency: a request with this key is still in progress"

// MsgKeyReused is the body of the 422 answered when a key comes back with another method or path.
const MsgKeyReused = "idempotency: this key was already used for a different request"
```

## 3. Behaviour (normative)

Errors are unexported typed constants (`type idemError string` + `Error()`), never `errors.New`.
`New` with nil `db` or `now` → `idempotency: db and now are required`.

The middleware, for every request:
1. Method is `GET`, `HEAD` or `OPTIONS`, **or** the header `router.HeaderIdempotencyKey` is absent
   or empty → call the next handler unchanged.
2. `id = ctx.UserID() + "\x1f" + key` (keys are scoped per user: a key sent by another user never
   reads someone else's stored response). Separator is an unexported constant.
3. Look up the record by `id`:
   - **not found** → insert `{id, method, path, state: "in_progress", created_at: now()}`; run the
     next handler through a **capturing context** (below); then update the record to
     `state: "done"` with the captured status (default 200 when the handler never called
     `WriteStatus`), body and content type. If the insert fails because another request inserted
     the same id first, answer as in "in_progress".
   - **found, `done`, same method and path** → write the stored `Content-Type` (if any), the stored
     status and the stored body; also set response header `Idempotent-Replayed: true` (unexported
     constant). **Do not call the handler.**
   - **found, `done`, different method or path** → `422` with body `MsgKeyReused`.
   - **found, `in_progress`, created less than 60 s ago** → `409` with body `MsgInProgress`.
   - **found, `in_progress`, created 60 s ago or more** (the first request crashed) → delete it and
     continue as "not found". The 60 s bound is an unexported constant.
4. Handler responses with status ≥ 500 are **not** stored: delete the in-progress record instead, so
   a retry runs again (a server failure is not an answer).

**Capturing context:** an unexported type embedding `router.Context` that overrides
`WriteStatus` (records the status, forwards), `Write` (appends to a buffer, forwards), `SetHeader`
(records `Content-Type`, forwards) and `Encode` (encodes with `webtyp.com/json` into bytes, sets
`Content-Type: application/json`, then goes through its own `Write`). Everything else is the
embedded context.

## 4. Stages

| Stage | Files | Content |
|---|---|---|
| 1 | `go.mod`, `model.go`, `model_orm.go` | `go get webtyp.com/router@v0.4.1 webtyp.com/orm webtyp.com/storage webtyp.com/model webtyp.com/json webtyp.com/fmt webtyp.com/ddl`; §1; run `ormc`; replace gonew's placeholder `idempotency.go` |
| 2 | `store.go`, `capture.go`, `errors.go` | §2, §3 |
| 3 | `migrate/migrate.go` | `Migrate` |
| 4 | `tests/` | §5 |
| 5 | `README.md`, `docs/ARCHITECTURE.md` | what it is, the rules of §3 as a table, "call `Use` before mounting routes", example below |

README example:
```go
store, _ := idempotency.New(orm.New(conn), time.Now)
srv.Router().Use(store.Middleware())          // BEFORE mounting routes
_ = rpc.Mount(srv.Router(), rpc.DefaultPrefix, modules...)
```

## 5. Tests (`tests/`, external package; `gotest`)

Consumer-shaped: a **real** `webtyp.com/server/httpd` server (read `server/httpd` tests for how to
build one in a test), an `Authn` that sets a fixed user from a test header, `store.Middleware()`
added with `Use` before the routes, a counting handler on `POST /thing` that writes `201` and a JSON
body, and plain `net/http` requests from the test. The backend is `orm.New(mem.New())` from
`webtyp.com/storage/mem` (v0.1.4 or later). If depending on `server/httpd` in the module is unwanted,
put the tests in a nested `tests/go.mod` module with `replace webtyp.com/idempotency => ../` (the
pattern `webtyp/rpc` and `webtyp/auth` use).

1. No header → handler runs every time (2 calls → count 2).
2. Same key twice → handler runs **once**; both answers are `201` with the same body; the second has
   `Idempotent-Replayed: true`.
3. Same key, different path → `422` + `MsgKeyReused`.
4. Same key from another user → handler runs again (scoped per user).
5. In progress: a handler that blocks on a channel; a second request with the same key while the
   first runs → `409` + `MsgInProgress`; release; a third request → replayed answer.
6. Stale in-progress (insert a record with `created_at = now - 61 s` directly through the store's
   `orm.DB`) → handler runs.
7. Handler answers `500` → nothing stored; a retry runs the handler again.
8. `GET` with a key → handler runs every time (not stored).
9. `Purge(before)` deletes only older `done` records.
10. `New(nil, ...)` → error.

## 6. Code rules (non-negotiable)
- Library code: `webtyp.com/fmt` for errors/formatting; no `errors`, `strconv`, `strings`,
  `encoding/json`; no `map`. Tests may use `net/http`.
- Repeated strings (states, header names, separator, messages) are constants.
- No exported symbol beyond §2 and the generated model. Tests in `tests/`; never export for a test.

## 7. Acceptance criteria
- `gotest ./...` green.
- `grep -rn "map\[" --include=*.go . | grep -v _test.go` → empty.
- **Before opening the PR:** `git diff --stat main` lists every file of the stages table, and no file
  is deleted that this plan does not order deleted.
