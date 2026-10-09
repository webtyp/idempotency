# Architecture

## Behaviour (normative)

Errors are unexported typed constants (`type idemError string` + `Error()`), never `errors.New`.
`New` with nil `db` or `now` → `idempotency: db and now are required`.

The middleware, for every request:
1. Method is `GET`, `HEAD` or `OPTIONS`, **or** the header `router.HeaderIdempotencyKey` is absent
   or empty → call the next handler unchanged.
2. `id = ctx.UserID() + "\x1f" + key` (keys are scoped per user: a key sent by another user never
   reads someone else's stored response). Separator is an unexported constant.
3. Look up the record by `id`:
   - **not found** → insert `{id, method, path, state: "in_progress", created_at: now()}`; run the
     next handler through a **capturing context**; then update the record to
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
