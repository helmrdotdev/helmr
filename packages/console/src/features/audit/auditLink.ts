import { hasPermission } from "../../lib/auth";
import type { Me } from "../../lib/auth";
import { useMe } from "../../lib/me";
import type { ActionMenuItem } from "../../ui/ActionMenu";

// Whether the viewer can read audit events anywhere in the organization.
export function canReadAudit(me: Me | undefined): boolean {
  return hasPermission(me, "audit_events.read") || !!me?.permission_scopes?.some((g) => g.permissions.includes("audit_events.read"));
}

// The audit page filtered to one object: what was done to it (`target`), or
// what a person or API key did (`actor`).
export function auditHref(filter: { target?: string | undefined; target_kind?: string | undefined; actor?: string | undefined }): string {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filter)) if (value) query.set(key, value);
  return `/settings/audit-events?${query}`;
}

// "View audit events" for an object's menu, for viewers who can read them.
export function useAuditItem(): (filter: { target?: string | undefined; target_kind?: string | undefined; actor?: string | undefined }) => ActionMenuItem[] {
  const me = useMe();
  return (filter) => (canReadAudit(me.data) ? [{ label: "View audit events", href: auditHref(filter) }] : []);
}
