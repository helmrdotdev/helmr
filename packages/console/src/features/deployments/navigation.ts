export function deploymentHref(id: string): string {
  return `/deployments/${encodeURIComponent(id)}`;
}
