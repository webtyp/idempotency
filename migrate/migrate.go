package migrate

import (
	"webtyp.com/ddl"
	"webtyp.com/idempotency"
)

func Migrate(conn ddl.Execer, compiler ddl.Compiler) error {
	return ddl.New(conn, compiler).CreateTable(&idempotency.Record{})
}
