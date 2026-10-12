package tests

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"webtyp.com/idempotency"
	"webtyp.com/model"
	"webtyp.com/orm"
	"webtyp.com/router"
	"webtyp.com/server/httpd"
	"webtyp.com/storage/mem"
)

const (
	headerUser = "X-Test-User"
	userA      = "user-a"
	userB      = "user-b"
	bodyThing  = `{"ok":true}`
	second     = int64(1000 * 1000 * 1000)
)

type env struct {
	ts      *httptest.Server
	store   *idempotency.Store
	db      *orm.DB
	calls   atomic.Int32 // runs of /thing, /other, /get and /fail
	release chan struct{}
	entered chan struct{}
}

func now() int64 { return time.Now().UnixNano() }

func setup(t *testing.T) *env { return setupClock(t, now) }

func setupClock(t *testing.T, clock func() int64) *env {
	t.Helper()
	e := &env{db: orm.New(mem.New()), release: make(chan struct{}), entered: make(chan struct{}, 1)}
	store, err := idempotency.New(e.db, clock)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.store = store

	srv := httpd.New(httpd.Config{
		TLS: httpd.TLSConfig{PlainHTTP: true},
		Authn: func(next router.HandlerFunc) router.HandlerFunc {
			return func(ctx router.Context) {
				ctx.SetUserID(ctx.GetHeader(headerUser))
				next(ctx)
			}
		},
		Authorize: func(string, model.Resource, model.Action) bool { return true },
	})
	r := srv.Router()
	r.Use(store.Middleware()) // BEFORE mounting the routes

	thing := func(ctx router.Context) {
		e.calls.Add(1)
		ctx.SetHeader("Content-Type", "application/json")
		ctx.WriteStatus(201)
		ctx.Write([]byte(bodyThing))
	}
	r.Post("/thing", thing).Requires("thing", model.Create)
	r.Post("/other", thing).Requires("thing", model.Create)
	r.Get("/get", thing).Requires("thing", model.Read)
	r.Post("/public", thing).Public()
	r.Post("/fail", func(ctx router.Context) {
		e.calls.Add(1)
		ctx.WriteStatus(500)
	}).Requires("thing", model.Create)
	r.Post("/block", func(ctx router.Context) {
		e.entered <- struct{}{}
		<-e.release
		e.calls.Add(1)
		ctx.WriteStatus(201)
		ctx.Write([]byte(bodyThing))
	}).Requires("thing", model.Create)

	h, err := srv.Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	e.ts = httptest.NewServer(h)
	t.Cleanup(e.ts.Close)
	return e
}

type answer struct {
	status   int
	body     string
	replayed string
}

func (e *env) do(t *testing.T, method, path, user, key string) answer {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.Header.Set(headerUser, user)
	}
	if key != "" {
		req.Header.Set(router.HeaderIdempotencyKey, key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, body: string(b), replayed: resp.Header.Get("Idempotent-Replayed")}
}

func (e *env) wantCalls(t *testing.T, n int32) {
	t.Helper()
	if got := e.calls.Load(); got != n {
		t.Fatalf("handler ran %d times, want %d", got, n)
	}
}

// 1
func TestNoHeaderRunsEveryTime(t *testing.T) {
	e := setup(t)
	e.do(t, "POST", "/thing", userA, "")
	e.do(t, "POST", "/thing", userA, "")
	e.wantCalls(t, 2)
}

// 2
func TestSameKeyRunsOnceAndReplays(t *testing.T) {
	e := setup(t)
	first := e.do(t, "POST", "/thing", userA, "k1")
	again := e.do(t, "POST", "/thing", userA, "k1")
	e.wantCalls(t, 1)
	if first.status != 201 || again.status != 201 {
		t.Fatalf("status %d / %d, want 201 / 201", first.status, again.status)
	}
	if first.body != bodyThing || again.body != bodyThing {
		t.Fatalf("bodies %q / %q, want %q", first.body, again.body, bodyThing)
	}
	if first.replayed != "" || again.replayed != "true" {
		t.Fatalf("Idempotent-Replayed %q / %q, want \"\" / \"true\"", first.replayed, again.replayed)
	}
}

// 3
func TestSameKeyOtherPathIs422(t *testing.T) {
	e := setup(t)
	e.do(t, "POST", "/thing", userA, "k1")
	got := e.do(t, "POST", "/other", userA, "k1")
	if got.status != 422 || got.body != idempotency.MsgKeyReused {
		t.Fatalf("got %d %q, want 422 %q", got.status, got.body, idempotency.MsgKeyReused)
	}
	e.wantCalls(t, 1)
}

// 4
func TestKeysAreScopedPerUser(t *testing.T) {
	e := setup(t)
	e.do(t, "POST", "/thing", userA, "k1")
	got := e.do(t, "POST", "/thing", userB, "k1")
	e.wantCalls(t, 2)
	if got.replayed != "" {
		t.Fatal("user B received user A's stored answer")
	}
}

// 5
func TestInProgressIs409ThenReplays(t *testing.T) {
	e := setup(t)
	done := make(chan answer)
	go func() { done <- e.do(t, "POST", "/block", userA, "k1") }()
	<-e.entered

	got := e.do(t, "POST", "/block", userA, "k1")
	if got.status != 409 || got.body != idempotency.MsgInProgress {
		t.Fatalf("got %d %q, want 409 %q", got.status, got.body, idempotency.MsgInProgress)
	}

	close(e.release)
	if first := <-done; first.status != 201 {
		t.Fatalf("first request: %d, want 201", first.status)
	}
	third := e.do(t, "POST", "/block", userA, "k1")
	if third.status != 201 || third.replayed != "true" {
		t.Fatalf("third request: %d replayed=%q, want 201 replayed", third.status, third.replayed)
	}
	e.wantCalls(t, 1)
}

// 5b: concurrent first requests with the same key run the handler once (mem has no PK check).
// The slow clock widens the gap between "look up" and "insert" so an unserialised store fails.
func TestConcurrentSameKeyRunsOnce(t *testing.T) {
	e := setupClock(t, func() int64 { time.Sleep(5 * time.Millisecond); return now() })
	close(e.release)
	e.entered = make(chan struct{}, 20)
	results := make(chan answer, 20)
	for i := 0; i < 20; i++ {
		go func() { results <- e.do(t, "POST", "/block", userA, "k1") }()
	}
	for i := 0; i < 20; i++ {
		<-results
	}
	e.wantCalls(t, 1)
}

// 6
func TestStaleInProgressRunsAgain(t *testing.T) {
	e := setup(t)
	stale := &idempotency.Record{Id: userA + "\x1f" + "k1", Method: "POST", Path: "/thing", State: "in_progress", CreatedAt: now() - 61*second}
	if err := e.db.Create(stale); err != nil {
		t.Fatal(err)
	}
	got := e.do(t, "POST", "/thing", userA, "k1")
	if got.status != 201 {
		t.Fatalf("got %d, want 201", got.status)
	}
	e.wantCalls(t, 1)
}

// 7
func TestServerErrorIsNotStored(t *testing.T) {
	e := setup(t)
	if got := e.do(t, "POST", "/fail", userA, "k1"); got.status != 500 {
		t.Fatalf("got %d, want 500", got.status)
	}
	e.do(t, "POST", "/fail", userA, "k1")
	e.wantCalls(t, 2)
}

// 8
func TestGetIsNeverStored(t *testing.T) {
	e := setup(t)
	e.do(t, "GET", "/get", userA, "k1")
	e.do(t, "GET", "/get", userA, "k1")
	e.wantCalls(t, 2)
}

// Anonymous callers share no scope: their keys are not stored.
func TestAnonymousPassesThrough(t *testing.T) {
	e := setup(t)
	e.do(t, "POST", "/public", "", "k1")
	got := e.do(t, "POST", "/public", "", "k1")
	e.wantCalls(t, 2)
	if got.replayed != "" {
		t.Fatal("anonymous caller received a stored answer")
	}
}

// 9
func TestPurge(t *testing.T) {
	e := setup(t)
	t0 := now()
	for _, r := range []*idempotency.Record{
		{Id: "old-done", Method: "POST", Path: "/a", State: "done", Status: 200, CreatedAt: t0 - 100},
		{Id: "new-done", Method: "POST", Path: "/b", State: "done", Status: 200, CreatedAt: t0 + 100},
		{Id: "fresh-running", Method: "POST", Path: "/c", State: "in_progress", CreatedAt: t0 - 100},
		{Id: "stale-running", Method: "POST", Path: "/d", State: "in_progress", CreatedAt: t0 - 61*second},
	} {
		if err := e.db.Create(r); err != nil {
			t.Fatal(err)
		}
	}
	n, err := e.store.Purge(t0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
	left, err := idempotency.ReadAllRecord(e.db.Query(&idempotency.Record{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("%d records left, want 2", len(left))
	}
	for _, r := range left {
		if r.Id != "new-done" && r.Id != "fresh-running" {
			t.Fatalf("record %q should have been purged", r.Id)
		}
	}
}

// 10
func TestNewRequiresDBAndNow(t *testing.T) {
	if _, err := idempotency.New(nil, now); err == nil {
		t.Fatal("New(nil db) returned no error")
	}
	if _, err := idempotency.New(orm.New(mem.New()), nil); err == nil {
		t.Fatal("New(nil now) returned no error")
	}
}
