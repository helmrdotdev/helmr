package audit

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
)

// ValidatePrivileges rejects ownership and reachable stronger roles, including
// NOINHERIT roles available through SET ROLE. Maintenance is a trusted expiry
// owner: DELETE is not claimed to enforce expiry against a compromised operator.
func ValidatePrivileges(ctx context.Context, database db.DBTX, maintenance bool) error {
	var allowed, unsafe bool
	err := database.QueryRow(ctx, `SELECT
  has_table_privilege(current_user,'public.audit_events','SELECT') AND
  has_table_privilege(current_user,'public.audit_events','INSERT') AND
  (NOT $1 OR has_table_privilege(current_user,'public.audit_events','DELETE')),
  EXISTS(SELECT 1 FROM pg_roles r WHERE (r.rolname=current_user OR pg_has_role(current_user,r.oid,'MEMBER')) AND (
   r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR
   r.oid=(SELECT relowner FROM pg_class WHERE oid='public.audit_events'::regclass) OR
   r.oid=(SELECT nspowner FROM pg_namespace WHERE nspname='public') OR
   r.oid=(SELECT datdba FROM pg_database WHERE datname=current_database()) OR
   has_schema_privilege(r.oid,'public','CREATE') OR
   has_table_privilege(r.oid,'public.audit_events','UPDATE,TRUNCATE,TRIGGER') OR
   has_any_column_privilege(r.oid,'public.audit_events','UPDATE') OR
   (NOT $1 AND has_table_privilege(r.oid,'public.audit_events','DELETE')) OR
   has_table_privilege(r.oid,'public.schema_migrations','INSERT,UPDATE,DELETE,TRUNCATE,TRIGGER') OR
   has_any_column_privilege(r.oid,'public.schema_migrations','UPDATE')
  ))`, maintenance).Scan(&allowed, &unsafe)
	if err != nil {
		return err
	}
	if !allowed || unsafe {
		return errors.New("database audit privileges require a separate migration owner and bounded runtime or maintenance grants")
	}
	return nil
}
