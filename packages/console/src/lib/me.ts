import { createQuery } from "@tanstack/solid-query";
import { getMe, hasPermission } from "./auth";

export const ME_QUERY_KEY = ["me"] as const;

// The signed-in account. Every screen reads it through this one query.
export function useMe() {
  return createQuery(() => ({ queryKey: ME_QUERY_KEY, queryFn: getMe, retry: false, staleTime: 60_000 }));
}

// Organization owners and admins manage members and connections.
export function isOrganizationAdmin(role: string | null | undefined): boolean {
  return role === "owner" || role === "admin";
}

export function usePermission() {
  const me = useMe();
  return {
    me,
    role: () => me.data?.role ?? null,
    isOwner: () => me.data?.role === "owner",
    isOrganizationAdmin: () => isOrganizationAdmin(me.data?.role),
    has: (permission: string) => hasPermission(me.data, permission),
  };
}
