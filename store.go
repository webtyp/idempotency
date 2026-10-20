package idempotency

import (
	"sync"

	"webtyp.com/orm"
	"webtyp.com/router"
	"webtyp.com/time"
)

// MsgInProgress is the exact body of the 409 answered while the first request with a key is
// still running; a client that retries treats it as "try later", not as a refusal.
const MsgInProgress = "idempotency: a request with this key is still in progress"

// MsgKeyReused is the body of the 422 answered when a key comes back with another method or path.
const MsgKeyReused = "idempotency: this key was already used for a different request"

var staleBound = time.Minute.Nano()

const (
	separator       = "\x1f"
	stateInProgress = "in_progress"
	stateDone       = "done"

	methodGet     = "GET"
	methodHead    = "HEAD"
	methodOptions = "OPTIONS"

	headerContentType = "Content-Type"
	headerReplayed    = "Idempotent-Replayed"
	replayedValue     = "true"

	statusDefault   = 200
	statusConflict  = 409
	statusReused    = 422
	statusServerErr = 500
)

// Store remembers responses by idempotency key.
type Store struct {
	db  *orm.DB
	now func() time.Nano
	// mu serialises "look up, then insert in_progress" inside this process, so two concurrent
	// requests with the same key can never both run the handler even on a backend that does not
	// enforce the primary key (storage/mem). Across processes the primary key does it.
	mu sync.Mutex
}

// New returns a store over db (the idempotency_record table must exist; see migrate).
// now is the clock (pass time.NowNano).
func New(db *orm.DB, now func() time.Nano) (*Store, error) {
	if db == nil || now == nil {
		return nil, errRequired
	}
	return &Store{db: db, now: now}, nil
}

// Purge deletes "done" records created before the given unix-ns instant, and "in_progress"
// records that are also stale (their request crashed and was never retried); returns how many.
func (s *Store) Purge(before time.Nano) (int, error) {
	staleBefore := s.now() - staleBound
	if before < staleBefore {
		staleBefore = before
	}
	n := 0
	for _, c := range []struct {
		state  string
		before time.Nano
	}{{stateDone, before}, {stateInProgress, staleBefore}} {
		qb := s.db.Query(&Record{}).Where(Record_.State).Eq(c.state).Where(Record_.CreatedAt).Lt(c.before)
		list, err := ReadAllRecord(qb)
		if err != nil {
			return n, err
		}
		if len(list) == 0 {
			continue
		}
		if err := s.db.Delete(&Record{}, orm.Eq(Record_.State, c.state), orm.Lt(Record_.CreatedAt, c.before)); err != nil {
			return n, err
		}
		n += len(list)
	}
	return n, nil
}

// Middleware returns the router.Middleware that applies the rules documented in
// docs/ARCHITECTURE.md. Call Router.Use with it BEFORE mounting the routes.
func (s *Store) Middleware() router.Middleware {
	return func(next router.HandlerFunc) router.HandlerFunc {
		return func(ctx router.Context) {
			method := ctx.Method()
			key := ctx.GetHeader(router.HeaderIdempotencyKey)
			// No identity means no scope to keep one caller's answer from another: pass through.
			if method == methodGet || method == methodHead || method == methodOptions || key == "" || ctx.UserID() == "" {
				next(ctx)
				return
			}
			id := ctx.UserID() + separator + key

			rec, out := s.claim(id, method, ctx.Path())
			switch out {
			case outStorage:
				answer(ctx, statusServerErr, msgStorage)
				return
			case outReused:
				answer(ctx, statusReused, MsgKeyReused)
				return
			case outInProgress:
				answer(ctx, statusConflict, MsgInProgress)
				return
			case outReplay:
				if rec.ContentType != "" {
					ctx.SetHeader(headerContentType, rec.ContentType)
				}
				ctx.SetHeader(headerReplayed, replayedValue)
				ctx.WriteStatus(int(rec.Status))
				ctx.Write(rec.Body)
				return
			}

			capture := &capturingContext{Context: ctx, status: statusDefault}
			next(capture)

			// The answer is already on the wire: a failed write below cannot change it. A record
			// left in_progress turns stale after staleBound and the next retry runs again.
			if capture.status >= statusServerErr {
				_ = s.db.Delete(&Record{}, orm.Eq(Record_.Id, id), orm.Eq(Record_.CreatedAt, int64(rec.CreatedAt)))
				return
			}
			rec.State = stateDone
			rec.Status = int64(capture.status)
			rec.Body = capture.body
			rec.ContentType = capture.contentType
			_ = s.db.UpdateFields(rec, []string{Record_.State, Record_.Status, Record_.Body, Record_.ContentType}, orm.Eq(Record_.Id, id))
		}
	}
}

// outcome is what claim decided for a request with a key.
type outcome uint8

const (
	outRun        outcome = iota // a new in_progress record is ours: run the handler
	outReplay                    // done with the same method and path: replay it
	outReused                    // done with another method or path: 422
	outInProgress                // still running (or lost an insert race): 409
	outStorage                   // the record table failed: 500
)

// claim looks the key up and, when nobody owns it, stores a new in_progress record. It only
// touches storage: the caller writes the response, outside the lock.
func (s *Store) claim(id, method, path string) (*Record, outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := &Record{}
	err := s.db.Query(found).Where(Record_.Id).Eq(id).ReadOne()
	now := s.now()
	switch {
	case err != nil && !orm.IsNotFound(err):
		return nil, outStorage
	case err == nil && found.State == stateDone:
		if found.Method != method || found.Path != path {
			return nil, outReused
		}
		return found, outReplay
	case err == nil && now-found.CreatedAt < staleBound:
		return nil, outInProgress
	case err == nil:
		// Stale in_progress: the first request crashed. Delete only that attempt.
		if err := s.db.Delete(&Record{}, orm.Eq(Record_.Id, id), orm.Eq(Record_.CreatedAt, int64(found.CreatedAt))); err != nil {
			return nil, outStorage
		}
	}

	rec := &Record{Id: id, Method: method, Path: path, State: stateInProgress, CreatedAt: now}
	if err := s.db.Create(rec); err != nil {
		// Another process inserted the same key first (primary key), or storage failed.
		return nil, outInProgress
	}
	return rec, outRun
}

func answer(ctx router.Context, status int, msg string) {
	ctx.WriteStatus(status)
	ctx.Write([]byte(msg))
}
