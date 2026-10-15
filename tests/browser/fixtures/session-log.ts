import type { Page } from "@playwright/test";

// A long-running Session served from memory with the real paging contracts of
// the Session, events, timeline, turns and asks endpoints.
export const projectID = "console-demo";
export const environmentID = "demo";
export const sessionID = "00000000-0000-7000-8000-000000000900";
export const base = `/api/projects/${projectID}/environments/${environmentID}`;
export const sessionPath = `${base}/sessions/${sessionID}`;
export const sessionURL = `/sessions/${sessionID}?project=${projectID}&environment=${environmentID}`;

type Status = "queued" | "running" | "finalizing" | "completed" | "failed" | "interrupted" | "cancelled";
export type Turn = {
  id: string; session_id: string; sequence: number; status: Status; input?: unknown; result?: unknown; response?: unknown;
  error?: { code: string; message?: string }; started_at?: string; terminal_at?: string; completion_save_id?: string; payload_expired_at?: string;
};
export type Event = { session_id: string; turn_id: string | null; sequence: number; kind: string; data: unknown; created_at: string };
export type Ask = Record<string, unknown> & { id: string; turn_id: string; status: string };

export type SessionData = {
  session: Record<string, unknown>;
  turns: Turn[];
  events: Event[];
  asks: Ask[];
  retainedAfter: number;
  // Fail this many event reads with 503 before answering again.
  failEvents?: number;
  // Delay this many event reads by `eventDelayMs` (real time, in the route handler).
  delayEvents?: number;
  eventDelayMs?: number;
};

const id = (n: number) => `00000000-0000-7000-8000-${String(n).padStart(12, "0")}`;
// Started two hours ago, so relative times and durations read naturally.
const start = Date.now() - 2 * 60 * 60 * 1000;
const at = (seconds: number) => new Date(start + seconds * 1000).toISOString();
export const turnID = (sequence: number) => id(10_000 + sequence);
export const askID = id(20_001);

const inputs = [
  "Check whether the flaky test in billing/export is still failing on main",
  "Open a draft PR with the retry fix and link the incident",
  "Run the full export suite against staging data",
  "Split shard 3 and retry only the slow tests",
  "Summarize open billing incidents",
];

// 42 Turns: #12 and #35 failed, #41 running and waiting on a question, #42 queued.
export function longSession(options: { turns?: number; expired?: boolean } = {}): SessionData {
  const count = options.turns ?? 42;
  const turns: Turn[] = [];
  const events: Event[] = [];
  let clock = 0;
  const push = (sequence: number | null, kind: string, data: unknown) => {
    clock += 7;
    events.push({ session_id: sessionID, turn_id: sequence === null ? null : turnID(sequence), sequence: events.length + 1, kind, data, created_at: at(clock) });
  };
  for (let sequence = 1; sequence <= count; sequence++) {
    const failed = sequence === 12 || sequence === 35;
    const running = sequence === count - 1;
    const queued = sequence === count;
    const input = [{ type: "text", text: `${inputs[sequence % inputs.length]} (#${sequence})` }];
    if (!queued) push(sequence, "message.admitted", { message_id: id(30_000 + sequence), status: "admitted", delivery: "admitted" });
    push(sequence, "turn.queued", {});
    const started = at(clock + 3);
    const turn: Turn = { id: turnID(sequence), session_id: sessionID, sequence, status: "queued", input };
    if (!queued) {
      push(sequence, "turn.running", {});
      push(sequence, "turn.output", [{ type: "text", text: `Running 214 tests in 6 shards.\nShard 3 is slower than expected.\nStep ${sequence}.` }]);
      push(sequence, "turn.output", [{ type: "json", value: { shard: 3, passed: 41, failed: failed ? 2 : 0, durations: Array.from({ length: 43 }, (_, i) => 900 + i * 13), meta: { host: "cmp-41f0", attempt: 1 } } }]);
      turn.started_at = started;
    }
    if (failed) {
      push(sequence, "turn.failed", { code: "handler_error" });
      Object.assign(turn, { status: "failed", error: { code: "handler_error", message: "Export job exceeded the 12m deadline (shard 3)" }, terminal_at: at(clock) });
    } else if (running) {
      push(sequence, "slack.delivery_unavailable", { reason: "route_unavailable", source_seq: events.length });
      push(sequence, "ask.created", { ask_id: askID, question: { title: "Merge PR #8812?", prompt: [{ type: "text", text: "CI is green on 9f2c1aa. The PR includes a schema migration." }], type: "choice", options: [{ id: "merge", label: "Merge now", description: "squash and delete the branch", value: "merge" }, { id: "wait", label: "Wait for review", value: "wait" }], allowNotes: true } });
      turn.status = "running";
    } else if (!queued) {
      push(sequence, "turn.completed", {});
      Object.assign(turn, { status: "completed", terminal_at: at(clock), completion_save_id: id(40_000 + sequence), result: { merged: false, checks: 14 }, response: [{ type: "text", text: `Done with step ${sequence}. All shards passed after the retry.` }] });
    }
    turns.push(turn);
  }
  const asks: Ask[] = [{
    id: askID, session_id: sessionID, turn_id: turnID(count - 1), status: "pending", created_at: at(clock),
    question: { title: "Merge PR #8812?", prompt: [{ type: "text", text: "CI is green on 9f2c1aa. The PR includes a schema migration." }], type: "choice", options: [{ id: "merge", label: "Merge now", value: "merge" }, { id: "wait", label: "Wait for review", value: "wait" }] },
  }];
  const expiredAt = options.expired ? at(clock + 60) : undefined;
  if (expiredAt) {
    for (const event of events) event.data = null;
    for (const turn of turns) {
      delete turn.input; delete turn.result; delete turn.response;
      if (turn.error) delete turn.error.message;
      turn.payload_expired_at = expiredAt;
      turn.terminal_at ??= expiredAt;
      if (turn.status === "running" || turn.status === "queued") turn.status = "cancelled";
    }
  }
  return {
    session: {
      activity: { process: { epoch: 1, status: "ready", applied: true, reconstruction_required: false, stop: null } },
      id: sessionID, agent: "triage", template: "node-22", deployment_id: id(501), computer_id: id(601), root_session_id: sessionID,
      parent_session_id: null, requester_session_id: null, initial_turn: { id: turnID(1), status: "completed" },
      slack_channel_id: "C08BILLING", status: options.expired ? "closed" : "open", created_at: at(0), holds: [],
      close_after_idle_ms: null, idle_since: null,
    },
    turns, events, asks, retainedAfter: options.expired ? events.length : 0,
  };
}

// Appends events for the running Turn, as if the agent produced more output.
export function appendOutput(data: SessionData, texts: string[]) {
  const running = data.turns.find(turn => turn.status === "running")!;
  for (const text of texts) {
    data.events.push({ session_id: sessionID, turn_id: running.id, sequence: data.events.length + 1, kind: "turn.output", data: [{ type: "text", text }], created_at: new Date().toISOString() });
  }
}

export type Requests = { events: string[]; turns: string[]; asks: string[] };

export async function serveSession(page: Page, data: SessionData, permissions: string[] = ["sessions.read", "sessions.interrupt", "sessions.cancel"]): Promise<Requests> {
  const requests: Requests = { events: [], turns: [], asks: [] };
  const environment = { environment: environmentID, project: projectID, name: "Demo", color_hex: "#315fce", is_default: true, created_at: at(0), updated_at: at(0) };
  const project = { project: projectID, name: "Console demo", default_region_id: "local", is_default: true, environments: [environment], created_at: at(0), updated_at: at(0) };
  await page.route("**/api/**", async route => {
    const url = new URL(route.request().url());
    const endpoint = url.pathname;
    const query = url.searchParams;
    const limit = Number(query.get("limit") ?? "100");
    const json = (body: unknown, status = 200) => route.fulfill({ status, json: body });
    if (endpoint === "/api/me") return json({ user_id: "local", display_name: "Local", org_id: "org", role: "owner", permissions, admin: false, organization_required: false, project_required: false });
    if (endpoint === "/api/projects") return json({ projects: [project] });
    if (endpoint === `/api/projects/${projectID}`) return json(project);
    if (endpoint === base) return json(environment);
    if (endpoint === sessionPath) return json(data.session);
    if (endpoint === `${sessionPath}/events`) {
      requests.events.push(url.search);
      if (data.delayEvents) {
        data.delayEvents--;
        await new Promise(resolve => setTimeout(resolve, data.eventDelayMs ?? 0));
      }
      if (data.failEvents) {
        data.failEvents--;
        return json({ error: { code: "unavailable", message: "unavailable" } }, 503);
      }
      const after = Number(query.get("after") ?? "0");
      if (after < data.retainedAfter) return json({ error: { code: "cursor_expired", message: "expired", details: { retained_after: data.retainedAfter } } }, 410);
      const records = data.events.filter(event => event.sequence > after).slice(0, limit);
      const next = records.at(-1)?.sequence ?? after;
      return json({ records, next_after: next, has_more: next < data.events.length, retained_after: data.retainedAfter });
    }
    if (endpoint === `${sessionPath}/turns`) {
      requests.turns.push(url.search);
      const after = Number(query.get("cursor") ?? "0");
      const page = data.turns.filter(turn => turn.sequence > after).slice(0, limit);
      const more = data.turns.some(turn => turn.sequence > (page.at(-1)?.sequence ?? after));
      return json({ turns: page, ...(more ? { next_cursor: String(page.at(-1)!.sequence) } : {}) });
    }
    const asks = new RegExp(`^${sessionPath}/turns/([^/]+)/asks$`).exec(endpoint);
    if (asks) {
      requests.asks.push(asks[1]!);
      const all = data.asks.filter(ask => ask.turn_id === asks[1]);
      const start = Number(query.get("cursor") ?? "0");
      const page = all.slice(start, start + limit);
      return json({ asks: page, ...(start + limit < all.length ? { next_cursor: String(start + limit) } : {}) });
    }
    const turn = new RegExp(`^${sessionPath}/turns/([^/]+)$`).exec(endpoint);
    if (turn) {
      const found = data.turns.find(item => item.id === turn[1]);
      return found ? json(found) : json({ error: { code: "forbidden", message: "denied" } }, 403);
    }
    if (endpoint.endsWith("/slack-delivery")) return json({ posts: [] });
    return json({ error: { code: "not_found", message: endpoint } }, 404);
  });
  return requests;
}

// A small Session for focused cases: Turns with their queued events, plus helpers
// that change a Turn or add an event the way the Control Plane would.
export function sessionWith(session: Record<string, unknown>, turns: Turn[]): SessionData {
  const data: SessionData = {
    session: {
      activity: { process: null }, id: sessionID, agent: "support", template: "browser", deployment_id: id(501), computer_id: id(601),
      root_session_id: sessionID, parent_session_id: null, requester_session_id: null, initial_turn: turns[0] ? { id: turns[0].id, status: turns[0].status } : null,
      status: "open", created_at: at(0), holds: [], close_after_idle_ms: null, idle_since: null, ...session,
    },
    turns: [], events: [], asks: [], retainedAfter: 0,
  };
  for (const turn of turns) {
    data.turns.push(turn);
    addEvent(data, turn.sequence, "turn.queued", {});
    if (turn.status !== "queued") addEvent(data, turn.sequence, `turn.${turn.status}`, turn.status === "failed" && turn.error ? { code: turn.error.code } : {});
  }
  return data;
}

export function addEvent(data: SessionData, turn: number | null, kind: string, payload: unknown) {
  const owner = turn === null ? undefined : data.turns.find(item => item.sequence === turn);
  data.events.push({ session_id: sessionID, turn_id: owner?.id ?? null, sequence: data.events.length + 1, kind, data: payload, created_at: new Date().toISOString() });
}

export function settleTurn(data: SessionData, sequence: number, patch: Partial<Turn>) {
  const turn = data.turns.find(item => item.sequence === sequence)!;
  Object.assign(turn, patch);
  if (patch.status) addEvent(data, sequence, `turn.${patch.status}`, {});
}

export function makeTurn(sequence: number, patch: Partial<Turn> = {}): Turn {
  return { id: turnID(sequence), session_id: sessionID, sequence, status: "queued", input: [{ type: "text", text: "original input" }], ...patch };
}

// Turns start closed; specs that read inside Turns open every loaded Turn.
export async function expandTurns(page: Page) {
  const log = page.getByRole("region", { name: "Log" });
  await log.locator("[data-turn-header]").first().waitFor({ timeout: 5_000 }).catch(() => undefined);
  const closed = log.locator('[data-turn-header] button[aria-expanded="false"]');
  while (await closed.count()) await closed.first().click();
}
