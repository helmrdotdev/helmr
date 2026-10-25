import { request } from "./api";
import type { AttentionItem } from "./attention";
import type { ComputerListItem } from "./computers";
import type { ScopeIDs } from "./scope-link";

export type OverviewTurns = { total: number; failed: number };

// One Agent's last 24 hours. `hourly` and `hourly_failed` hold 24 one-hour
// buckets counted back from the read, oldest first.
export type OverviewAgent = {
  agent: string;
  active: number;
  turns: number;
  failed: number;
  last_turn_at: string | null;
  hourly: number[];
  hourly_failed: number[];
};

export type OverviewSessionState = "held" | "running" | "queued";
export type OverviewOrigin = "slack" | "user" | "api_key" | "trigger" | "session";

// A Session with work in progress; `since` is when that state began.
export type OverviewSession = {
  id: string;
  agent: string;
  state: OverviewSessionState;
  since: string;
  origin: OverviewOrigin;
  slack_channel_id?: string;
};

// The window's failed Turns of one Agent with one error code.
export type OverviewFailure = {
  agent: string;
  error_code: string;
  count: number;
  last_hour: number;
  first_at: string;
  last_at: string;
  last_session_id: string;
  last_turn_id: string;
  last_turn_sequence: number;
};

// What the caller can read of one Environment's Sessions: Turns admitted in the
// last 24 hours and the 24 before, activity by Agent, Sessions with work in
// progress (all counted, the first few listed), failures by cause, and the
// current deployment's share for callers who can read deployments.
export type Overview = {
  turns: OverviewTurns;
  previous: OverviewTurns;
  agents: OverviewAgent[];
  active: { count: number; running: number; queued: number; held: number; sessions: OverviewSession[] };
  failures: OverviewFailure[];
  deployment: { id: string; turns: OverviewTurns; older_open_sessions: number } | null;
};

export function getOverview(scope: ScopeIDs): Promise<Overview> {
  return request<Overview>(`/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/overview`);
}

// The share of Turns that failed, as a percentage to one decimal; undefined
// without Turns.
export function failureRate(turns: OverviewTurns): number | undefined {
  return turns.total ? Math.round((turns.failed / turns.total) * 1000) / 10 : undefined;
}

// A question counts as left too long after this.
export const UNANSWERED_AFTER_MS = 2 * 60 * 60 * 1000;

export type IssueTone = "danger" | "warning" | "neutral";
export type IssueTarget = { kind: "turn"; sessionID: string; turnID: string } | { kind: "session"; sessionID: string } | { kind: "computers" } | { kind: "slack"; agent: string } | { kind: "question"; item: AttentionItem };

// One problem on the Overview: what is wrong, why it matters, and where to act.
export type Issue = { id: string; tone: IssueTone; title: string; detail: string; action: string; target: IssueTarget };

const plural = (count: number, one: string, other: string) => `${count} ${count === 1 ? one : other}`;

// The problems that need someone, most urgent first, grouped by cause: an
// Agent failing in the last hour, Computers that cannot run, held Sessions,
// Slack connections that lost authorization, and questions left too long.
// Each source is optional; a part the caller cannot read adds nothing.
export function overviewIssues(input: {
  now: number;
  failures?: OverviewFailure[] | undefined;
  computers?: ComputerListItem[] | undefined;
  attention?: AttentionItem[] | undefined;
}): Issue[] {
  const issues: Issue[] = [];
  for (const f of input.failures ?? []) {
    if (!f.last_hour) continue;
    issues.push({
      id: `failure:${f.agent}:${f.error_code}`,
      tone: "danger",
      title: `${f.agent} is failing · ${plural(f.last_hour, "turn", "turns")} in the last hour`,
      detail: `${f.error_code || "No error code"} · ${plural(f.count, "failure", "failures")} in 24 hours · latest turn #${f.last_turn_sequence}`,
      action: "Inspect",
      target: { kind: "turn", sessionID: f.last_session_id, turnID: f.last_turn_id },
    });
  }
  const computers = input.computers ?? [];
  const waiting = computers.filter((c) => c.status === "available" && c.execution_wait);
  if (waiting.length) {
    const limit = waiting.some((c) => c.execution_wait?.reason === "environment_limit");
    issues.push({
      id: "computers:waiting",
      tone: "warning",
      title: `${plural(waiting.length, "computer is", "computers are")} waiting for capacity`,
      detail: limit ? "The environment's computer limit is reached. New turns wait until a computer sleeps or closes." : "New turns wait until capacity is available.",
      action: "View",
      target: { kind: "computers" },
    });
  }
  const unavailable = computers.filter((c) => c.status === "available" && c.residency === "unavailable");
  if (unavailable.length) {
    issues.push({
      id: "computers:unavailable",
      tone: "warning",
      title: `${plural(unavailable.length, "computer is", "computers are")} unavailable`,
      detail: "Nothing can run on them until they are recreated.",
      action: "View",
      target: { kind: "computers" },
    });
  }
  const attention = input.attention ?? [];
  for (const hold of attention.filter((item) => item.kind === "hold" && item.session_id)) {
    issues.push({
      id: `hold:${hold.id}`,
      tone: "warning",
      title: "A session is held",
      detail: `${hold.agent} · ${hold.reason || "Waiting to be resumed"}`,
      action: "Open",
      target: { kind: "session", sessionID: hold.session_id! },
    });
  }
  for (const lost of attention.filter((item) => item.kind === "slack_reauthorization")) {
    issues.push({
      id: `slack:${lost.id}`,
      tone: "warning",
      title: "Slack connection needs reauthorization",
      detail: `${lost.agent}${lost.workspace_name ? ` · ${lost.workspace_name}` : ""} · replies are not delivered to Slack until then`,
      action: "Reauthorize",
      target: { kind: "slack", agent: lost.agent },
    });
  }
  const stale = attention.filter((item) => item.kind === "question" && input.now - Date.parse(item.at) > UNANSWERED_AFTER_MS);
  if (stale.length) {
    const agents = [...new Set(stale.map((item) => item.agent))];
    issues.push({
      id: "questions:stale",
      tone: "neutral",
      title: `${plural(stale.length, "question", "questions")} unanswered for over 2 hours`,
      detail: `Asked by ${agents.join(", ")}`,
      action: "Answer",
      target: { kind: "question", item: stale[0]! },
    });
  }
  return issues;
}

const COMPUTER_ORDER: Record<string, number> = { unavailable: 0, starting: 2, restoring: 2, running: 3, parking: 4, parked: 5, cold: 6 };

// Computers that need a look first, then awake ones, each most recently active
// first.
export function computerRank(c: ComputerListItem): number {
  return c.execution_wait ? 1 : (COMPUTER_ORDER[c.residency] ?? 7);
}

export function orderComputers(computers: readonly ComputerListItem[]): ComputerListItem[] {
  return computers
    .filter((c) => c.status === "available")
    .slice()
    .sort((a, b) => computerRank(a) - computerRank(b) || Date.parse(b.last_activity_at) - Date.parse(a.last_activity_at));
}
