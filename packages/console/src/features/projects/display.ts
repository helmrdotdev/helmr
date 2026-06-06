export type EnvironmentTone = "danger" | "warning" | "info" | "purple" | "success" | "neutral";

export function envTone(slug: string | undefined | null): EnvironmentTone {
  if (!slug) return "neutral";
  const s = slug.toLowerCase();
  if (/(prod|production|live|main|master)/.test(s)) return "info";
  if (/(stag|staging|qa|preview)/.test(s)) return "purple";
  if (/(dev|develop|local|sandbox|test)/.test(s)) return "info";
  if (/(demo|playground)/.test(s)) return "success";
  return "neutral";
}
