import { createQuery } from "@tanstack/solid-query";
import { getMe, hasPermission } from "./auth";

export const ME_QUERY_KEY = ["me"] as const;

// The signed-in account. Every screen reads it through this one query.
export function useMe() {
  return createQuery(() => ({ queryKey: ME_QUERY_KEY, queryFn: getMe, retry: false, staleTime: 60_000 }));
}

export function usePermission() {
  const me = useMe();
  return {
    me,
    isOwner: () => me.data?.is_owner === true,
    has: (permission: string, scope?: Parameters<typeof hasPermission>[2]) => hasPermission(me.data, permission, scope),
  };
}
