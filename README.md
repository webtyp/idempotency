# idempotency
<img src="docs/img/badges.svg">

Server side of the `Idempotency-Key` header: a `router.Middleware` that runs a retried request
**once** and answers every retry with the stored response. Useful for any retried non-GET request:
an offline outbox resending its queue, a double click on Save, a webhook delivered twice.

The header name is `router.HeaderIdempotencyKey`; the client side is `rpc.Caller.CallKeyed`.

## Usage

```go
store, _ := idempotency.New(orm.New(conn), time.Now)
srv.Router().Use(store.Middleware())          // BEFORE mounting routes
_ = rpc.Mount(srv.Router(), rpc.DefaultPrefix, modules...)
```

- Call `Use` **before** mounting the routes: `httpd` captures the middleware list when each route
  is registered.
- `Use` middlewares run behind authentication and the RBAC gate, so a rejected request never
  touches the store.
- Create the table with `migrate.Migrate(conn, compiler)`.
- Call `store.Purge(now - 24h)` periodically (Stripe keeps keys 24 h).

## Rules

| Request | Answer |
|---|---|
| `GET`, `HEAD`, `OPTIONS`, no key, or anonymous caller | handler runs, nothing stored |
| new key | handler runs; its answer is stored (status, body, `Content-Type`) |
| same key, same method and path, answered | stored answer, header `Idempotent-Replayed: true`; handler **not** run |
| same key, other method or path | `422` + `MsgKeyReused` |
| same key, still running (< 60 s) | `409` + `MsgInProgress` (retry later) |
| same key, running for 60 s or more | the first attempt crashed: handler runs again |
| handler answered `5xx` | nothing stored: a retry runs again |

Keys are scoped per user: one user's key never reads another user's answer. Details and
limitations: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
