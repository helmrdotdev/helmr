export function workspaceHref(id: string): string {
  return `/workspaces/${encodeURIComponent(id)}`;
}
