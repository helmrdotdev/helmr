// Package db exposes schema-derived persistence; workflow decisions belong to
// domain packages.
//
// Data access follows one rule. Every statement a production package sends to
// PostgreSQL is either a generated sqlc query in this package or a compile-time
// constant at its call site in the owning package. The constants are parsed
// against the fresh schema by internal/sqlpolicy, which also keeps the
// exceptions (migration runners and the few statements composed at runtime)
// and the table ownership list: each table has one package that writes it, and
// a generated query file belongs to the owner whose writes it performs. Reads
// may cross owners. Models are generated only for tables a generated query
// reads; owners that write their own SQL scan into their own types.
//
// RevokedComputerSecretsSQL is the one shared relation; its sqlc copy in
// query/allocation_planning.sql is kept identical by a test here.
package db
