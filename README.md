# idempotency

Idempotency middleware that runs a retried request once and replays its stored response.

## Usage

Ensure `Use` is called before mounting the routes.

```go
store, _ := idempotency.New(orm.New(conn), time.Now)
srv.Router().Use(store.Middleware())          // BEFORE mounting routes
_ = rpc.Mount(srv.Router(), rpc.DefaultPrefix, modules...)
```
