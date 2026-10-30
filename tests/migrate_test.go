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