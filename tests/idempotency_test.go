package tests

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"webtyp.com/idempotency"
	"webtyp.com/idempotency/migrate"
	"webtyp.com/orm"
	"webtyp.com/router"
	"webtyp.com/server/httpd"
	"webtyp.com/storage/mem"
	"webtyp.com/ddl"
)

type mockAuthn struct{}

func (m mockAuthn) Authenticate(r *http.Request) (string, error) {
	user := r.Header.Get("Test-User")
	if user == "" {
		return "user1", nil
	}
	return user, nil
}

func setupTestServer(t *testing.T) (*httpd.Server, *idempotency.Store, *orm.DB, *int) {
	conn := mem.New()
	db := orm.New(conn)

	err := migrate.Migrate(conn, ddl.PostgresCompiler{}) // use dummy compiler
	if err != nil && err.Error() != "not implemented" {
		// Ignore compiler error if mem driver doesn't need it or it doesn't fail
	}

	// For the sake of the test, let's create the table directly if needed
	_, _ = db.Exec("CREATE TABLE idempotency_record (id TEXT PRIMARY KEY, method TEXT NOT NULL, path TEXT NOT NULL, state TEXT NOT NULL, status INTEGER NOT NULL, body BLOB, content_type TEXT, created_at INTEGER NOT NULL)")

	nowFunc := func() int64 { return time.Now().UnixNano() }
	store, err := idempotency.New(db, nowFunc)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	srv := httpd.New(httpd.Config{
		Authn: mockAuthn{},
	})

	srv.Router().Use(store.Middleware())

	callCount := 0

	srv.Router().Post("/thing", func(ctx router.Context) {
		callCount++
		ctx.WriteStatus(201)
		ctx.SetHeader("Content-Type", "application/json")
		ctx.Write([]byte(`{"success":true}`))
	})

	srv.Router().Post("/other", func(ctx router.Context) {
		ctx.WriteStatus(200)
		ctx.Write([]byte(`{"other":true}`))
	})

	blockCh := make(chan struct{})
	srv.Router().Post("/block", func(ctx router.Context) {
		<-blockCh
		ctx.WriteStatus(200)
		ctx.Write([]byte(`{"blocked":false}`))
	})

	srv.Router().Post("/error", func(ctx router.Context) {
		ctx.WriteStatus(500)
	})

	srv.Router().Get("/get", func(ctx router.Context) {
		callCount++
		ctx.WriteStatus(200)
	})

	return srv, store, db, &callCount
}

func TestNoHeader(t *testing.T) {
	srv, _, _, callCount := setupTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/thing", nil)
	client := &http.Client{}

	resp1, _ := client.Do(req)
	resp2, _ := client.Do(req)

	if resp1.StatusCode != 201 || resp2.StatusCode != 201 {
		t.Errorf("expected 201, got %v and %v", resp1.StatusCode, resp2.StatusCode)
	}
	if *callCount != 2 {
		t.Errorf("expected 2 calls, got %v", *callCount)
	}
}

func TestSameKeyTwice(t *testing.T) {
	srv, _, _, callCount := setupTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/thing", nil)
	req.Header.Set("Idempotency-Key", "key1")
	client := &http.Client{}

	resp1, _ := client.Do(req)
	resp2, _ := client.Do(req)

	if resp1.StatusCode != 201 || resp2.StatusCode != 201 {
		t.Errorf("expected 201, got %v and %v", resp1.StatusCode, resp2.StatusCode)
	}
	if *callCount != 1 {
		t.Errorf("expected 1 call, got %v", *callCount)
	}
	if resp2.Header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("expected Idempotent-Replayed header on second response")
	}
}

func TestSameKeyDifferentPath(t *testing.T) {
	srv, _, _, _ := setupTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req1, _ := http.NewRequest("POST", ts.URL+"/thing", nil)
	req1.Header.Set("Idempotency-Key", "key2")
	client := &http.Client{}
	client.Do(req1)

	req2, _ := http.NewRequest("POST", ts.URL+"/other", nil)
	req2.Header.Set("Idempotency-Key", "key2")
	resp2, _ := client.Do(req2)

	if resp2.StatusCode != 422 {
		t.Errorf("expected 422, got %v", resp2.StatusCode)
	}

	buf := new(bytes.Buffer)
	buf.ReadFrom(resp2.Body)
	if buf.String() != idempotency.MsgKeyReused {
		t.Errorf("expected %v, got %v", idempotency.MsgKeyReused, buf.String())
	}
}

func TestSameKeyAnotherUser(t *testing.T) {
	srv, _, _, callCount := setupTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req1, _ := http.NewRequest("POST", ts.URL+"/thing", nil)
	req1.Header.Set("Idempotency-Key", "key3")
	req1.Header.Set("Test-User", "userA")
	client := &http.Client{}
	client.Do(req1)

	req2, _ := http.NewRequest("POST", ts.URL+"/thing", nil)
	req2.Header.Set("Idempotency-Key", "key3")
	req2.Header.Set("Test-User", "userB")
	client.Do(req2)

	if *callCount != 2 {
		t.Errorf("expected 2 calls, got %v", *callCount)
	}
}

func TestHandler500(t *testing.T) {
	srv, _, db, _ := setupTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/error", nil)
	req.Header.Set("Idempotency-Key", "key5")
	client := &http.Client{}

	resp1, _ := client.Do(req)
	if resp1.StatusCode != 500 {
		t.Errorf("expected 500, got %v", resp1.StatusCode)
	}

	var count int
	db.QueryRow("SELECT count(*) FROM idempotency_record WHERE id = ?", "user1\x1fkey5").Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 records, got %v", count)
	}
}

func TestGetWithKey(t *testing.T) {
	srv, _, _, callCount := setupTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/get", nil)
	req.Header.Set("Idempotency-Key", "key6")
	client := &http.Client{}

	client.Do(req)
	client.Do(req)

	if *callCount != 2 {
		t.Errorf("expected 2 calls, got %v", *callCount)
	}
}

func TestPurge(t *testing.T) {
	_, store, db, _ := setupTestServer(t)

	now := time.Now().UnixNano()

	db.Exec("INSERT INTO idempotency_record (id, method, path, state, status, created_at) VALUES (?, ?, ?, ?, ?, ?)", "id1", "POST", "/a", "done", 200, now - 100)
	db.Exec("INSERT INTO idempotency_record (id, method, path, state, status, created_at) VALUES (?, ?, ?, ?, ?, ?)", "id2", "POST", "/b", "done", 200, now + 100)
	db.Exec("INSERT INTO idempotency_record (id, method, path, state, status, created_at) VALUES (?, ?, ?, ?, ?, ?)", "id3", "POST", "/c", "in_progress", 200, now - 100)

	deleted, _ := store.Purge(now)

	if deleted != 1 {
		t.Errorf("expected to delete 1 record, deleted %v", deleted)
	}
}

func TestNewWithNil(t *testing.T) {
	_, err := idempotency.New(nil, time.Now)
	if err == nil {
		t.Errorf("expected error, got nil")
	}
	if err.Error() != "idempotency: db and now are required" {
		t.Errorf("unexpected error message: %v", err)
	}
}
