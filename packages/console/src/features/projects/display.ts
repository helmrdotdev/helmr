export const ENVIRONMENT_COLOR_PRESETS = [
  "#315FCE",
  "#0EA5E9",
  "#F59E0B",
  "#22C55E",
  "#06B6D4",
  "#8B5CF6",
  "#EC4899",
  "#F97316",
  "#14B8A6",
  "#6366F1",
] as const;

const CUSTOM_ENVIRONMENT_COLOR_PRESETS = [
  "#0EA5E9",
  "#8B5CF6",
  "#EC4899",
  "#F97316",
  "#14B8A6",
  "#84CC16",
  "#6366F1",
] as const;

export function defaultEnvironmentColor(slug: string | undefined | null): string {
  const s = slug?.trim().toLowerCase() ?? "";
  if (!s) return CUSTOM_ENVIRONMENT_COLOR_PRESETS[0];
  if (/(prod|production|live|main|master)/.test(s)) return "#315FCE";
  if (/(stag|staging|qa)/.test(s)) return "#F59E0B";
  if (/preview/.test(s)) return "#06B6D4";
  if (/(dev|develop|local)/.test(s)) return "#22C55E";
  if (/(sandbox|test)/.test(s)) return "#8B5CF6";
  if (/(demo|playground)/.test(s)) return "#8B5CF6";
  return colorFromSlug(s);
}

export function normalizeEnvironmentColor(colorHex: string): string {
  return colorHex.trim().toUpperCase();
}

function colorFromSlug(slug: string): (typeof CUSTOM_ENVIRONMENT_COLOR_PRESETS)[number] {
  let hash = 0;
  for (let index = 0; index < slug.length; index += 1) {
    hash = (hash * 31 + slug.charCodeAt(index)) >>> 0;
  }
  return CUSTOM_ENVIRONMENT_COLOR_PRESETS[hash % CUSTOM_ENVIRONMENT_COLOR_PRESETS.length]!;
}
