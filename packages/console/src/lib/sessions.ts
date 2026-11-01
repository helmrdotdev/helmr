import { parseAsk, parseAskPage } from "../../../../sdk/typescript/src/internal/asks";
import type { JsonValue } from "../../../../sdk/typescript/src/contract";
import { postJson, request, selectedOrganization, expectedActorHeaders } from "./api";

export type SessionStatus = "open" | "closing" | "closed";
type ProcessStop = { process_epoch: number; turn_id: string | null; state: "requested" | "stopping" | "stopped"; deadline: string };
export type Session = {
  related_via?: "initiated" | "contributed" | "answered" | "descendant";
  permissions?: string[];
  agent: string;
  template: string;
  activity: {
    process: { epoch: number; status: string; applied: boolean; reconstruction_required: boolean; stop: ProcessStop | null } | null;
  };
  slack_channel_id?: string | null;
  id: string;
  deployment_id: string;
  computer_id: string | null;
  // Null when the root session is not readable.
  root_session_id: string | null;
  parent_session_id: string | null;
  requester_session_id: string | null;
  initial_turn: { id: string; status: SessionTurn["status"] } | null;
  key?: string;
  status: SessionStatus;
  close_after_idle_ms: number | null;
  created_at: string;
  holds: SessionHold[];
};
// A hold in effect for a session. A hold on an ancestor that is not readable
// has null identifiers. Resume posts to session_id when can_release is true.
export type SessionHold = {
  id: string | null;
  session_id: string | null;
  kind: "interrupt" | "process_failed" | "recovery_gap" | "storage_pressure";
  recovery_gap?: RecoveryGap;
  created_at: string;
  can_release: boolean;
};
export type SessionTurn = {
  stop?: ProcessStop;
  id: string;
  session_id: string;
  sequence: number;
  status:
    | "queued"
    | "running"
    | "completed"
    | "failed"
    | "interrupted"
    | "cancelled";
  input?: JsonValue;
  payload_expired_at?: string;
  result?: unknown;
  error?: { code: string; message?: string };
  response?: import("../../../../sdk/typescript/src/content").Content;
  started_at?: string;
  terminal_at?: string;
};
type SessionTurnPage = { turns: SessionTurn[]; next_cursor?: string };
export type SessionEvent = {
  session_id: string;
  turn_id: string | null;
  sequence: number;
  kind: string;
  data: JsonValue;
  created_at: string;
};
type SessionEventPage = {
  records: SessionEvent[];
  next_after: number;
  has_more: boolean;
  retained_after: number;
};
type SessionEventPageOptions = {
  after?: number | undefined;
  limit?: number | undefined;
};
export type SessionReceipt = {
  id?: string;
  sequence?: number;
  status?: string;
  kind?: string;
  session_id?: string;
  turn_id?: string | null;
  message_id?: string;
  hold_id?: string;
};
export type SessionAddress = {
  sessionID: string;
  projectID: string;
  environmentID: string;
};
type ListSessionsResponse = {
  sessions: Session[];
  next_cursor?: string;
};

export async function listSessions(options: {
  relatedUserID?: string | undefined;
  projectID: string;
  environmentID: string;
  agent?: string | undefined;
  statuses?: SessionStatus[] | undefined;
  cursor?: string | undefined;
  limit?: number | undefined;
}): Promise<ListSessionsResponse> {
  if (!options.projectID || !options.environmentID) {
    throw new Error("Session project and environment are required");
  }
  const params = new URLSearchParams();
  if (options.relatedUserID) params.set("related_user_id",options.relatedUserID);
  if (options.agent) params.set("agent", options.agent);
  for (const status of options.statuses ?? []) params.append("status", status);
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.size === 0 ? "" : `?${params.toString()}`;
  return request<ListSessionsResponse>(
    `/api/projects/${encodeURIComponent(options.projectID)}/environments/${encodeURIComponent(options.environmentID)}/sessions${query}`,
  );
}

export async function getSession(address: SessionAddress): Promise<Session> {
  return request<Session>(sessionAPIPath(address));
}

export function getSessionEvents(
  address: SessionAddress,
  options: SessionEventPageOptions = {},
): Promise<SessionEventPage> {
  return request(`${sessionAPIPath(address)}/events${eventPageQuery(options)}`);
}

export function interruptSession(
  address: SessionAddress,
  input: { idempotency_key: string },
): Promise<SessionReceipt> {
  return postJson(
    `${sessionAPIPath(address)}/interrupt`,
    input,
  );
}

export function sessionConsolePath(sessionID: string): string {
  return `/sessions/${encodeURIComponent(sessionID)}`;
}

function eventPageQuery(options: SessionEventPageOptions): string {
  const params = new URLSearchParams();
  if (options.after !== undefined) params.set("after", String(options.after));
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  return params.size === 0 ? "" : `?${params.toString()}`;
}

function sessionAPIPath(address: SessionAddress): string {
  if (!address.projectID || !address.environmentID) {
    throw new Error("Session project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(address.projectID)}/environments/${encodeURIComponent(address.environmentID)}/sessions/${encodeURIComponent(address.sessionID)}`;
}

export function listSessionTurns(
  address: SessionAddress,
  options: { cursor?: string | undefined; limit?: number | undefined } = {},
): Promise<SessionTurnPage> {
  const params = new URLSearchParams();
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  return request(
    `${sessionAPIPath(address)}/turns${params.size ? `?${params}` : ""}`,
  );
}
export function cancelSession(
  address: SessionAddress,
  input: { idempotency_key: string },
): Promise<SessionReceipt> {
  return postJson(`${sessionAPIPath(address)}/cancel`, input);
}

export type AskAddress = SessionAddress & { turnID: string; askID: string };
// Quote every argument, including route-provided IDs, for a POSIX shell.
export function questionCLICommands(address: AskAddress, origin: string) {
  const quote = (value: string) => "'" + value.replaceAll("'", "'\"'\"'") + "'";
  const prefix = `helmr --api-url ${quote(origin)} session turn ask`;
  const target = `${quote(address.askID)} --session ${quote(address.sessionID)} --turn ${quote(address.turnID)}`;
  const scope = `--project ${quote(address.projectID)} --env ${quote(address.environmentID)}`;
  return {
    login: `helmr login ${quote(origin)}`,
    get: `${prefix} get ${target} ${scope} --json`,
    answer: `${prefix} answer ${target} ${scope} --answer-file answer.json --answer-id ANSWER_ID`,
  };
}

export function askConsolePath(address: Pick<AskAddress, "sessionID" | "turnID" | "askID">): string {
  return `${sessionConsolePath(address.sessionID)}/turns/${encodeURIComponent(address.turnID)}/asks/${encodeURIComponent(address.askID)}`;
}

export async function getAsk(address: AskAddress) {
  return parseAsk(await request(`${askAPIPath(address)}/${encodeURIComponent(address.askID)}`));
}
export async function listAsks(address: SessionAddress & { turnID: string }, cursor?: string) {
  return parseAskPage(await request(`${askAPIPath(address)}?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`));
}

function askAPIPath(address: SessionAddress & { turnID: string }) {
  return `${sessionAPIPath(address)}/turns/${encodeURIComponent(address.turnID)}/asks`;
}

export function messageDeliveryNotice(event: SessionEvent): string | undefined {
 if (event.kind !== "message.rejected" || event.data === null || typeof event.data !== "object" || Array.isArray(event.data)) return undefined;
 return "delivery" in event.data && event.data["delivery"] === "uncertain"
  ? "Message delivery uncertain. Check the conversation before sending it again."
  : "Message not delivered. Send a new message when the agent is available.";
}

export function interruptTurn(address: SessionAddress, turnID: string, input: { idempotency_key: string }): Promise<SessionReceipt> {
  return postJson(`${sessionAPIPath(address)}/turns/${encodeURIComponent(turnID)}/interrupt`, input);
}

// Reconnection always refreshes durable snapshots; hints need no event cursor.
export function watchSession(address: SessionAddress, changed: () => void): () => void {
  const controller = new AbortController();
  const organization = selectedOrganization();
  const actor = expectedActorHeaders();
  let retry: ReturnType<typeof setTimeout> | undefined;
  const connect = async () => {
    try {
      const response = await fetch(`${sessionAPIPath(address)}/updates`, {
        credentials: "same-origin", signal: controller.signal,
        headers: { Accept: "text/event-stream", "X-Helmr-Organization": organization, ...actor },
        cache: "no-store",
      });
      if (response.status === 401 || response.status === 403 || response.status === 404 || response.status === 409) {
        changed(); // Refresh the authoritative view and its actionable auth state.
        return;
      }
      if (!response.ok || !response.body) throw new Error("Update stream unavailable");
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let pending = "";
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          pending += decoder.decode(value, { stream: true });
          if (pending.length > 65536) throw new Error("Invalid update frame");
          let end: number;
          while ((end = pending.indexOf("\n\n")) !== -1) {
            const frame = pending.slice(0, end);
            pending = pending.slice(end + 2);
            if (frame.split("\n").some((line) => line === "event: change")) changed();
          }
        }
      } finally {
        await reader.cancel().catch(() => {});
        reader.releaseLock();
      }
    } catch {
      // Network interruptions reconnect; explicit authority refusals above stop.
    }
    if (!controller.signal.aborted) retry = setTimeout(() => void connect(), 2000);
  };
  void connect();
  return () => { controller.abort(); if (retry !== undefined) clearTimeout(retry); };
}

export type RecoveryGap = {
  holdId: string | null;
  computerId: string;
  leaseEpoch: number;
  recoverySaveId: string | null;
  restoredAt: string | null;
  affectedTurnIds: string[];
};

export function recoveryGapNotice(gap: RecoveryGap): string {
  const point = gap.restoredAt ? new Date(gap.restoredAt).toLocaleString() : "the initial work environment";
  if (gap.holdId === null) return `This computer was restored to ${point}. File changes made after that point may be missing. No completed requests in this session were affected, and new requests can run.`;
  return `This computer was restored to ${point}. File changes and native conversation history for ${gap.affectedTurnIds.length} completed requests in this conversation may be missing. Replies remain available. Completed requests will not run again.`;
}

// What a hold means for the session it is shown on.
export function holdNotice(hold: SessionHold, sessionID: string): string {
  const inherited = hold.session_id !== sessionID;
  switch (hold.kind) {
    case "interrupt": return inherited ? "Held by an interrupt on a parent session." : "Held by an interrupt. Queued work waits until it is resumed.";
    case "process_failed": return "Held after the session process failed repeatedly. Queued work waits until it is resumed.";
    case "recovery_gap": return hold.recovery_gap ? recoveryGapNotice(hold.recovery_gap) : "Held after the computer was restored with missing file changes.";
    case "storage_pressure": return "Held while storage is full. Queued work resumes when space is available. Stopped requests will not restart automatically.";
  }
}

export function resumeSession(address: SessionAddress, input: { idempotency_key: string; hold_id: string }): Promise<SessionReceipt> {
  return postJson(`${sessionAPIPath(address)}/resume`, input);
}

export function storagePressureNotice(event: SessionEvent): string | undefined {
  if (event.kind !== "session.storage_pressure" || event.data === null || typeof event.data !== "object" || Array.isArray(event.data)) return undefined;
  switch ("state" in event.data ? event.data["state"] : undefined) {
    case "paused": return "Work was paused while storage was full.";
    case "stopped": return "The request was stopped because storage stayed full. Local changes are retained while this environment remains available; recovery may omit changes not yet saved.";
    case "resolved": return "Storage recovered and queued work was allowed to continue. Stopped requests were not restarted automatically.";
    default: return undefined;
  }
}

// The number of sessions a close would close: this session and the sessions it
// owns that are not closed yet.
export function getSessionClosePreview(address: SessionAddress): Promise<{ sessions: number }> {
  return request(`${sessionAPIPath(address)}/close-preview`);
}
export function closeSession(address: SessionAddress, input: { idempotency_key: string }): Promise<SessionReceipt> {
  return postJson(`${sessionAPIPath(address)}/close`, input);
}
