package schema

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// immutableColumns are columns the runtime role sets on insert and can never
// update. An organization worker group's owner is fixed at creation.
var immutableColumns = map[string][]string{
	"worker_groups": {"owner_org_id"},
}

// ConfigurePrivileges runs only with the migration credential after migrations.
// Grants cover existing relations; future tables receive no implicit privileges.
func ConfigurePrivileges(ctx context.Context, migrationURL, runtimeRole, maintenanceRole string) error {
	conn, err := pgx.Connect(ctx, migrationURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	owner := conn.Config().User
	if runtimeRole == "" || maintenanceRole == "" || runtimeRole == owner || maintenanceRole == owner || runtimeRole == maintenanceRole {
		return errors.New("migration, runtime and audit maintenance roles must be distinct")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	runtime := pgx.Identifier{runtimeRole}.Sanitize()
	maintenance := pgx.Identifier{maintenanceRole}.Sanitize()
	rows, err := tx.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, table)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, table := range tables {
		name := pgx.Identifier{"public", table}.Sanitize()
		if _, err := tx.Exec(ctx, "REVOKE ALL ON TABLE "+name+" FROM PUBLIC, "+runtime+", "+maintenance); err != nil {
			return err
		}
		switch table {
		case "schema_migrations":
			if _, err := tx.Exec(ctx, "GRANT SELECT ON TABLE "+name+" TO "+runtime); err != nil {
				return err
			}
		case "audit_events":
			if _, err := tx.Exec(ctx, "GRANT SELECT, INSERT ON TABLE "+name+" TO "+runtime); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "GRANT SELECT, INSERT, DELETE ON TABLE "+name+" TO "+maintenance); err != nil {
				return err
			}
		default:
			fixed, ok := immutableColumns[table]
			if !ok {
				if _, err := tx.Exec(ctx, "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "+name+" TO "+runtime); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, "GRANT SELECT, INSERT, DELETE ON TABLE "+name+" TO "+runtime); err != nil {
				return err
			}
			var updatable string
			if err := tx.QueryRow(ctx, `SELECT string_agg(quote_ident(column_name), ', ' ORDER BY ordinal_position)
  FROM information_schema.columns
 WHERE table_schema='public' AND table_name=$1 AND is_generated='NEVER' AND NOT (column_name = ANY($2))`, table, fixed).Scan(&updatable); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "GRANT UPDATE ("+updatable+") ON TABLE "+name+" TO "+runtime); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
