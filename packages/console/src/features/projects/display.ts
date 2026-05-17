export function envTone(slug: string | undefined | null): "danger" | "warning" | "info" | "success" | "neutral" {
  if (!slug) return "neutral";
  const s = slug.toLowerCase();
  if (/(prod|production|live|main|master)/.test(s)) return "danger";
  if (/(stag|staging|qa|preview)/.test(s)) return "warning";
  if (/(dev|develop|local|sandbox|test)/.test(s)) return "info";
  if (/(demo|playground)/.test(s)) return "success";
  return "neutral";
}
