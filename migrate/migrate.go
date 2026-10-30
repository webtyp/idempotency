package migrate

import (
	"webtyp.com/ddl"
	"webtyp.com/idempotency"
)

func Migrate(conn ddl.Execer, compiler ddl.Compiler) error {
	// Sync, not CreateTable: it creates a missing table and also adds the columns a model
	// gained, so a new field reaches a database that already exists. Flow: https://github.com/webtyp/ddl/blob/main/docs/diagrams/SYNC_FLOW.md
	return ddl.New(conn, compiler).Sync(
		&idempotency.Record{},
	)
}
