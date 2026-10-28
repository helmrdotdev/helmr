import { hasPermission } from "../../lib/auth";
import type { Me } from "../../lib/auth";
import { useMe } from "../../lib/me";
import type { ActionMenuItem } from "../../ui/ActionMenu";

// Whether the viewer can read audit events anywhere in the organization.
export function canReadAudit(me: Me | undefined): boolean {
  return hasPermission(me, "audit_events.read") || !!me?.permission_scopes?.some((g) => g.permissions.includes("audit_events.read"));
}

type AuditLinkFilter = { target?: string | undefined; target_kind?: string | undefined; actor?: string | undefined };

// The audit page filtered to one object: what was done to it (`target`), or
// what a person or API key did (`actor`). Admin objects open the admin audit page.
export function auditHref(filter: AuditLinkFilter, page = "/settings/audit-events"): string {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(filter)) if (value) query.set(key, value);
  return `${page}?${query}`;
}

// "View audit events" for an object's menu, for viewers who can read them.
export function useAuditItem(): (filter: AuditLinkFilter) => ActionMenuItem[] {
  const me = useMe();
  return (filter) => (canReadAudit(me.data) ? [{ label: "View audit events", href: auditHref(filter) }] : []);
}

// The same item for platform objects in the admin area, which only Helmr
// administrators open.
export function adminAuditItem(filter: AuditLinkFilter): ActionMenuItem {
  return { label: "View audit events", href: auditHref(filter, "/admin/audit-events") };
}
