---
PLAN: "fix: migrate with ddl.Sync so a new field reaches an existing table"
EXECUTOR: jules
REVIEWER: none
STATUS: running
SESSION: 12464057737415319270
---

> Read AGENTS.md at the repo root before any change.

# Migrate with `ddl.Sync`

## Context

`migrate.Migrate` creates this repo's tables with `ddl.DB.CreateTable`, which compiles to `CREATE TABLE IF NOT EXISTS`:
against a table that already exists it does nothing. So when a model gains a field, a database that is already
installed never receives the new column, and the first read or write of that field fails in production.
`ddl.DB.Sync` creates the missing table **and** adds the missing columns (it also renames a column the model declares
as renamed, and drops a column only when it holds no data). Its flow, branch by branch, is this diagram — the tests
below walk its branches: https://github.com/webtyp/ddl/blob/main/docs/diagrams/SYNC_FLOW.md

Rule decided for the whole ecosystem: nobody writes migration files; every library and module migrates with `Sync`.

## Tools

```bash
go install webtyp.com/devflow/cmd/gotest@latest
go get webtyp.com/ddl@v0.0.36
go mod tidy
```

Environment: `GOPRIVATE=webtyp.com/*,github.com/veltylabs/*`, `GOFLAGS=-mod=mod`.

## APIs used

| Symbol | Exact signature | Tag |
|---|---|---|
| `ddl.New` | `func New(conn Execer, ddlCompiler Compiler) *DB` | `webtyp.com/ddl` v0.0.36 |
| `(*ddl.DB).Sync` | `func (d *DB) Sync(models ...model.Model) error` — models in the order given; a connection without transactions syncs model by model | v0.0.36 |
| `ddl.Execer` | `interface { Exec(query string, args ...any) error }` | v0.0.36 |
| `ddl.Compiler` | `interface { CompileDDL(s Stmt, m model.Model) (query string, args []any, err error) }` | v0.0.36 |
| `ddl.TableIntrospector` | `interface { TableColumns(table string) ([]string, error) }` — when the connection has it, `Sync` adds only the missing columns; when `TableColumns` fails, `Sync` returns that same error and touches no later model | v0.0.36 |
| `ddl.Stmt` | `struct { Op Op; Table string; Database string; Column *model.Field; OldName string; ColumnName string }` | v0.0.36 |
| `ddl.OpCreateTable`, `ddl.OpAddColumn` | constants of type `ddl.Op` | v0.0.36 |
| `model.Model` | has `ModelName() string` and `Schema() []model.Field`; `model.Field.Name string` | `webtyp.com/model` (already required) |

If a signature in the published tag differs from this table, stop and name the symbol in the PR; never adapt it by guessing.

## Stage 1 — tests first: `tests/migrate_test.go` (new file)

Use the package name the other files in `tests/` already use (`package tests` if the folder is new). The tests are
written **before** the code: run `gotest` after this stage and see `TestMigrate_NewField_AddsOnlyThatColumn` fail
(today the migration never adds a column). Write exactly this file:

```go
package tests

import (
	"errors"
	"testing"

	"webtyp.com/ddl"
	"webtyp.com/model"

	"webtyp.com/idempotency"
	"webtyp.com/idempotency/migrate"
)

// migrateConn is the database the migration sees: Exec does nothing, and
// TableColumns answers which columns each table already has.
type migrateConn struct {
	existing  []tableColumns
	failTable string
}

type tableColumns struct {
	table   string
	columns []string
}

var errIntrospect = errors.New("introspection failed")

func (c *migrateConn) Exec(string, ...any) error { return nil }

func (c *migrateConn) TableColumns(table string) ([]string, error) {
	if table == c.failTable {
		return nil, errIntrospect
	}
	for _, t := range c.existing {
		if t.table == table {
			return t.columns, nil
		}
	}
	return nil, nil
}

// recordingCompiler keeps, in order, every statement the migration asked for.
type recordingCompiler struct{ stmts []ddl.Stmt }

func (r *recordingCompiler) CompileDDL(s ddl.Stmt, _ model.Model) (string, []any, error) {
	r.stmts = append(r.stmts, s)
	return "", nil, nil
}

// migrationModels are this repo's tables in foreign-key order: the order Migrate uses.
func migrationModels() []model.Model {
	return []model.Model{&idempotency.Record{}}
}

func columnsOf(m model.Model) []string {
	var out []string
	for _, f := range m.Schema() {
		out = append(out, f.Name)
	}
	return out
}

func upToDate() []tableColumns {
	var out []tableColumns
	for _, m := range migrationModels() {
		out = append(out, tableColumns{table: m.ModelName(), columns: columnsOf(m)})
	}
	return out
}

// Branch "listing works, nothing missing": only the idempotent CREATE TABLE per table, in order.
func TestMigrate_UpToDate_OnlyEnsuresTables(t *testing.T) {
	comp := &recordingCompiler{}
	if err := migrate.Migrate(&migrateConn{existing: upToDate()}, comp); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	models := migrationModels()
	if len(comp.stmts) != len(models) {
		t.Fatalf("got %d statements, want %d: %+v", len(comp.stmts), len(models), comp.stmts)
	}
	for i, m := range models {
		if comp.stmts[i].Op != ddl.OpCreateTable || comp.stmts[i].Table != m.ModelName() {
			t.Errorf("statement %d: got %+v, want CREATE TABLE %s (foreign-key order)", i, comp.stmts[i], m.ModelName())
		}
	}
}

// Branch "field missing from the table": exactly one ADD COLUMN, for that field of that table.
func TestMigrate_NewField_AddsOnlyThatColumn(t *testing.T) {
	models := migrationModels()
	last := models[len(models)-1]
	cols := columnsOf(last)
	existing := upToDate()
	existing[len(existing)-1].columns = cols[:len(cols)-1]

	comp := &recordingCompiler{}
	if err := migrate.Migrate(&migrateConn{existing: existing}, comp); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var adds []ddl.Stmt
	for _, s := range comp.stmts {
		if s.Op == ddl.OpAddColumn {
			adds = append(adds, s)
		}
	}
	if len(adds) != 1 {
		t.Fatalf("got %d ADD COLUMN, want 1: %+v", len(adds), adds)
	}
	if adds[0].Table != last.ModelName() || adds[0].Column == nil || adds[0].Column.Name != cols[len(cols)-1] {
		t.Fatalf("got %+v, want ADD COLUMN %s.%s", adds[0], last.ModelName(), cols[len(cols)-1])
	}
}

// Branch "listing the columns failed": the error comes back and no later table is touched.
func TestMigrate_IntrospectionFails_ReturnsErrorAndStops(t *testing.T) {
	first := migrationModels()[0]
	comp := &recordingCompiler{}
	err := migrate.Migrate(&migrateConn{existing: upToDate(), failTable: first.ModelName()}, comp)
	if !errors.Is(err, errIntrospect) {
		t.Fatalf("got %v, want the introspection error", err)
	}
	for _, s := range comp.stmts {
		if s.Table != first.ModelName() {
			t.Fatalf("touched %s after the failure on %s", s.Table, first.ModelName())
		}
	}
}
```


Commit after this stage (the red test is expected here).

## Stage 2 — `migrate/migrate.go`

Replace **only the body** of `Migrate`, keeping its signature, its parameter names and the existing doc comments
(package and function). The body becomes exactly:

```go
	// Sync, not CreateTable: it creates a missing table and also adds the columns a model
	// gained, so a new field reaches a database that already exists. Flow: https://github.com/webtyp/ddl/blob/main/docs/diagrams/SYNC_FLOW.md
	return ddl.New(conn, compiler).Sync(
		&idempotency.Record{},
	)
```

Remove any import that becomes unused. Run `gotest`: the three tests pass. Commit after this stage.

## Stage 3 — docs

If `README.md` or any `docs/*.md` says the tables are created with `CreateTable` (or "creates the tables" without
mentioning that new columns are added), change it to say that `migrate.Migrate` uses `ddl.Sync`, in English, and
link the diagram https://github.com/webtyp/ddl/blob/main/docs/diagrams/SYNC_FLOW.md. If no doc mentions the migration, add one line to `README.md` under its usage or
installation section saying so. Commit after this stage.

## Delivery check

1. `gotest` passes (it builds, vets and runs every test). A PR that does not compile, or whose tests could not run, is a failed delivery.
2. `grep -rn "CreateTable(" --include=*.go . | grep -v _test.go` → empty.
3. `git diff --stat main` lists `tests/migrate_test.go`, `migrate/migrate.go`, `go.mod`, `go.sum`, the docs touched — nothing else deleted.

## Delivery rules

- Never pause to ask permission or report progress: when the delivery check passes, commit, push and open the PR. Stuck
  for 30 minutes: open the PR anyway, listing what fails.
- Commit after every stage. Never run `git reset`, `git restore`, `git checkout -- <file>` or `git stash` on your own work.
- Never edit a generated file (`model_orm.go`): a build error there means the hand-written code is wrong.
- Do not change the models, their order, or any other package.

## Stages

| # | Files | Result |
|---|---|---|
| 1 | `tests/migrate_test.go` | Three tests, one per branch; the new-field test is red |
| 2 | `migrate/migrate.go` | `Migrate` uses `Sync`; all green |
| 3 | `README.md` / `docs/*` | The migration is documented with the diagram |
