package idempotency

import (
	"webtyp.com/orm"
	"webtyp.com/router"
)

const MsgInProgress = "idempotency: a request with this key is still in progress"
const MsgKeyReused = "idempotency: this key was already used for a different request"

const separator = "\x1f"
const stateInProgress = "in_progress"
const stateDone = "done"
const staleBound int64 = 60 * 1000 * 1000 * 1000 // 60 seconds in nanoseconds

type Store struct {
	db  *orm.DB
	now func() int64
}

func New(db *orm.DB, now func() int64) (*Store, error) {
	if db == nil || now == nil {
		return nil, errMissingDBAndNow
	}
	return &Store{db: db, now: now}, nil
}

func (s *Store) Purge(before int64) (int, error) {
	count, err := s.db.Delete(&Record{}).Where("state = ? AND created_at < ?", stateDone, before).Run()
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

func (s *Store) Middleware() router.Middleware {
	return func(next router.HandlerFunc) router.HandlerFunc {
		return func(ctx router.Context) {
			method := ctx.Method()
			if method == "GET" || method == "HEAD" || method == "OPTIONS" {
				next(ctx)
				return
			}
			key := ctx.GetHeader(router.HeaderIdempotencyKey)
			if key == "" {
				next(ctx)
				return
			}

			id := ctx.UserID() + separator + key
			now := s.now()

			var rec Record
			err := s.db.Select(&rec).Where("id = ?", id).One()

			if err != nil && err != orm.ErrNotFound {
				// Unexpected error from DB
				ctx.WriteStatus(500)
				return
			}

			if err == nil {
				// Record found
				if rec.State == stateDone {
					if rec.Method != method || rec.Path != ctx.Path() {
						ctx.WriteStatus(422)
						ctx.Write([]byte(MsgKeyReused))
						return
					}
					if rec.ContentType != "" {
						ctx.SetHeader("Content-Type", rec.ContentType)
					}
					ctx.SetHeader("Idempotent-Replayed", "true")
					ctx.WriteStatus(rec.Status)
					ctx.Write(rec.Body)
					return
				}

				if rec.State == stateInProgress {
					if now-int64(rec.CreatedAt) < staleBound {
						ctx.WriteStatus(409)
						ctx.Write([]byte(MsgInProgress))
						return
					}
					// Stale in_progress
					s.db.Delete(&Record{}).Where("id = ?", id).Run()
					// Continue as not found
				}
			}

			// Insert new record (or answer 409 if race condition)
			newRec := Record{
				ID:        id,
				Method:    method,
				Path:      ctx.Path(),
				State:     stateInProgress,
				CreatedAt: int(now),
			}
			err = s.db.Insert(&newRec).Run()
			if err != nil {
				// Assume duplicate key error, meaning another request raced us
				ctx.WriteStatus(409)
				ctx.Write([]byte(MsgInProgress))
				return
			}

			captContext := &capturingContext{
				Context: ctx,
				status:  200,
			}

			next(captContext)

			if captContext.status >= 500 {
				s.db.Delete(&Record{}).Where("id = ?", id).Run()
				return
			}

			// Update to done
			s.db.Update(&Record{}).Set("state", stateDone).Set("status", captContext.status).Set("body", captContext.body).Set("content_type", captContext.contentType).Where("id = ?", id).Run()
		}
	}
}
