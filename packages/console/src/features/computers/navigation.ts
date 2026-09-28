export function computerHref(id: string): string {
  return `/computers/${encodeURIComponent(id)}`;
}
