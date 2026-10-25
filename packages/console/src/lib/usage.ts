import { request } from "./api";
import type { ScopeIDs } from "./scope-link";

export type UsageRange = 7 | 30 | 90;

// How much the organization uses an Environment's agents over a window of
// days, from the Sessions the caller can read. Daily arrays hold one value per
// day counted back from the read, oldest first.
export type Usage = {
  days: number;
  work: { seconds: number; previous_seconds: number; turns: number; previous_turns: number; daily: number[] };
  people: { weekly: number; members: number | null; slack: number; app: number; daily: number[]; channels: { id: string; name?: string; people: number }[] };
  runs: { bounds: number[]; buckets: number[]; median_seconds: number; completed: number; unassisted: number; longest: { agent: string; seconds: number; session_id: string; turn_id: string } | null };
  agents: { agent: string; seconds: number; people: number }[];
};

export function getUsage(scope: ScopeIDs, days: UsageRange): Promise<Usage> {
  return request<Usage>(`/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/usage?range=${days}d`);
}

// A length of time in the largest unit that keeps it short: "40 s", "6 min",
// "2.5 h", "312 h".
export function formatDuration(seconds: number): string {
  if (seconds < 60) return `${Math.round(seconds)} s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)} min`;
  const hours = seconds / 3600;
  return hours < 10 ? `${Math.round(hours * 10) / 10} h` : `${Math.round(hours).toLocaleString("en-US")} h`;
}

// The change from the previous window as a signed whole percentage; undefined
// when there is nothing to compare with.
export function change(current: number, previous: number): string | undefined {
  if (!previous) return undefined;
  const percent = Math.round(((current - previous) / previous) * 100);
  return `${percent > 0 ? "+" : ""}${percent}%`;
}

// Labels for the run-length buckets from their upper bounds in seconds.
export function bucketLabels(bounds: readonly number[]): string[] {
  const label = (s: number) => (s < 3600 ? `${s / 60}m` : `${s / 3600}h`);
  return [`<${label(bounds[0]!)}`, ...bounds.slice(1).map((b, i) => `${label(bounds[i]!)}–${label(b)}`), `${label(bounds.at(-1)!)}+`];
}

// A Slack conversation's label: its name, "Direct messages" for a DM, or its
// ID until Helmr has seen its name.
export function channelLabel(channel: { id: string; name?: string }): string {
  if (channel.name) return `# ${channel.name}`;
  return channel.id.startsWith("D") ? "Direct messages" : channel.id;
}
