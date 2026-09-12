export function deploymentHref(id: string): string {
  return `/deployments/${encodeURIComponent(id)}`;
}

export function shortID(id: string): string {
  return id.slice(0, 8);
}

export function shortDigest(digest: string): string {
  if (!digest) return "—";
  const [algorithm, value] = digest.split(":", 2);
  if (!value) return digest.length > 18 ? `${digest.slice(0, 18)}…` : digest;
  return `${algorithm}:${value.slice(0, 12)}`;
}
