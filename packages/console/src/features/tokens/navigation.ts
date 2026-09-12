export function tokenHref(id: string): string {
  return `/tokens/${encodeURIComponent(id)}`;
}
