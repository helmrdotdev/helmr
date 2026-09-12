const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const UNITS = [
  { name: "year", value: 365 * DAY },
  { name: "month", value: 30 * DAY },
  { name: "day", value: DAY },
  { name: "hour", value: HOUR },
  { name: "minute", value: MINUTE },
];

export function formatRelative(iso: string | null | undefined): string {
  if (!iso) return "—";
  const time = new Date(iso).getTime();
  if (Number.isNaN(time)) return "—";
  const diff = Date.now() - time;
  if (Math.abs(diff) < 45_000) return diff >= 0 ? "just now" : "in a moment";
  const unit = UNITS.find((candidate) => Math.abs(diff) >= candidate.value) ?? UNITS[UNITS.length - 1]!;
  const count = Math.max(1, Math.round(Math.abs(diff) / unit.value));
  const label = count === 1 ? unit.name : `${unit.name}s`;
  return diff >= 0 ? `${count} ${label} ago` : `in ${count} ${label}`;
}

export function formatAbsolute(iso: string | null | undefined): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(date);
}
