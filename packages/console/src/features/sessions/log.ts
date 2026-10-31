import type { JsonValue, SessionEventKind } from "../../../../../sdk/typescript/src/contract";
import type { Content } from "../../../../../sdk/typescript/src/content";
import type { NormalizedQuestion } from "../../../../../sdk/typescript/src/question";
import { parseSessionEventData } from "../../../../../sdk/typescript/src/internal/session-event";
import { messageDeliveryNotice, recoveryGapNotice, storagePressureNotice, type RecoveryGap, type SessionEvent, type SessionTurn } from "../../lib/sessions";
import { formatID } from "../../ui/id";
import { byteLength, preview } from "../../ui/payload";
import type { Tone } from "../../ui/styles";

export type LogView = "all" | "output" | "problems";

// What the agent produced or asked.
export const isOutput = (kind: string) => kind === "turn.output" || kind.startsWith("ask.");

const problems = new Set(["turn.failed", "turn.interrupted", "message.rejected", "slack.delivery_unavailable", "session.process_failed"]);
export const isProblem = (kind: string) => problems.has(kind);

export function eventTone(kind: string): Tone {
  if (isProblem(kind)) return "danger";
  if (kind.startsWith("ask.") || kind === "session.storage_pressure" || kind === "session.recovery_gap") return "warning";
  if (kind === "turn.output") return "info";
  if (kind === "turn.completed") return "success";
  return "neutral";
}

// The typed data of an event, or its raw JSON when it does not parse (an event
// kind newer than this Console still renders).
export type ParsedEvent =
  | { kind: "turn.output"; content: Content }
  | { kind: "ask.created"; askID: string; question: NormalizedQuestion }
  | { kind: "other"; data: Record<string, JsonValue> | undefined };

export function parseEvent(event: SessionEvent): ParsedEvent {
  if (event.data === null) return { kind: "other", data: undefined };
  try {
    // The parser validates; display keeps the data as sent (key order included).
    const data = parseSessionEventData(event.kind as SessionEventKind, event.data) as unknown;
    if (event.kind === "turn.output") return { kind: "turn.output", content: event.data as unknown as Content };
    if (event.kind === "ask.created") {
      const value = data as { ask_id: string; question: NormalizedQuestion };
      return { kind: "ask.created", askID: value.ask_id, question: value.question };
    }
    return { kind: "other", data: data as Record<string, JsonValue> };
  } catch {
    return { kind: "other", data: typeof event.data === "object" && !Array.isArray(event.data) ? event.data as Record<string, JsonValue> : undefined };
  }
}

const words: Record<string, string> = {
  "turn.queued": "queued",
  "turn.running": "running",
  "turn.completed": "completed",
  "turn.interrupted": "interrupted",
  "turn.cancelled": "cancelled",
  "turn.interrupt": "interrupt requested",
  "session.interrupt": "session interrupt requested",
  "session.resume": "session resume requested",
  "session.close": "session close requested",
  "session.closed": "session closed",
  "session.process_stopped": "process stopped",
  "ask.cancelled": "question cancelled",
};

const shortID = (id: unknown) => (typeof id === "string" ? formatID(id) : "");

export function firstLine(text: string, budget = 140): string {
  const line = text.trimStart().split("\n", 1)[0] ?? "";
  return line.length > budget ? `${line.slice(0, budget - 1)}…` : line;
}

export function contentSummary(content: Content): string {
  return content.map(part => part.type === "text" ? `"${firstLine(part.text)}"` : `json ${preview(part.value)}`).join(" · ");
}

export function questionSummary(question: NormalizedQuestion): string {
  const text = question.prompt.find(part => part.type === "text");
  const head = question.title ?? (text?.type === "text" ? firstLine(text.text) : "Question");
  if (question.type === "text") return `${head} · text answer`;
  return `${head} · ${question.multiple ? "choose any" : "choice"}: ${question.options.map(option => option.label).join(" / ")}`;
}

// One line describing an event, without its kind (shown beside it).
export function eventSummary(event: SessionEvent, parsed = parseEvent(event)): string {
  if (event.data === null) return "content expired";
  if (parsed.kind === "turn.output") return contentSummary(parsed.content);
  if (parsed.kind === "ask.created") return questionSummary(parsed.question);
  const data = parsed.data ?? {};
  switch (event.kind) {
    case "session.storage_pressure": return storagePressureNotice(event) ?? preview(data);
    case "turn.failed": return typeof data["code"] === "string" ? data["code"] : "failed";
    case "ask.answered": return `answered by ${data["answered_by_user_id"] ? "a user" : "an API key"} · ${preview(data["answer"] ?? null)}`;
    case "message.admitted": case "message.started": case "message.delivered":
      return `message ${shortID(data["message_id"])} ${event.kind.slice("message.".length)}`;
    case "message.rejected":
      return `${messageDeliveryNotice(event) ?? "Message not delivered."}${typeof data["reason"] === "string" ? ` · ${data["reason"]}` : ""}`;
    case "slack.delivery_unavailable": return `Slack update for #${String(data["source_seq"] ?? "?")} not posted · ${String(data["reason"] ?? "")}`;
    case "session.process_failed": return `process ${String(data["processEpoch"] ?? "")} failed${typeof data["reason"] === "string" ? ` · ${data["reason"]}` : ""}${typeof data["holdId"] === "string" ? "; work is held" : "; queued work continues"}`;
    case "turn.interrupted":
      if (data["reason"] === "turn_deadline_elapsed") return "stopped after the maximum turn duration; queued work continues";
      break;
    case "session.recovery_gap": return recoveryGapNotice(data as unknown as RecoveryGap);
    case "session.closed": return ({ close: "session closed", cancel: "session cancelled", idle: "session closed after idle" } as Record<string, string>)[String(data["how"])] ?? "session closed";
  }
  return words[event.kind] ?? preview(data);
}

// How an opened line shows its record: which nodes start open, and which keys
// its summary line uses (marked in the tree). One rule per kind; a kind without
// one opens `data`. Paths are keys joined by "." from the record's root.
type RecordRule = { open: string[]; pinned: (data: Record<string, JsonValue> | JsonValue[]) => string[] };
const parts = (data: Record<string, JsonValue> | JsonValue[]) => (Array.isArray(data) ? data : []);
const recordRules: Record<string, RecordRule> = {
  "turn.output": {
    open: ["data", "data.*", "data.*.value"],
    pinned: data => parts(data).map((part, index) => `data.${index}.${(part as { type?: unknown } | null)?.type === "json" ? "value" : "text"}`),
  },
  "turn.failed": { open: ["data"], pinned: () => ["data.code"] },
  "ask.created": {
    open: ["data", "data.question", "data.question.options"],
    pinned: data => {
      const question = Array.isArray(data) ? undefined : (data as { question?: { options?: unknown } }).question;
      const options = Array.isArray(question?.options) ? question.options : [];
      return ["data.question.title", "data.question.type", ...options.map((_, index) => `data.question.options.${index}.label`)];
    },
  },
  "ask.answered": { open: ["data", "data.answer", "data.answer.selected"], pinned: () => ["data.answer"] },
  "slack.delivery_unavailable": { open: ["data"], pinned: () => ["data.reason", "data.source_seq"] },
  "session.process_failed": { open: ["data"], pinned: () => ["data.processEpoch", "data.reason", "data.holdId"] },
};
for (const kind of ["message.admitted", "message.started", "message.delivered", "message.rejected"])
  recordRules[kind] = { open: ["data"], pinned: () => ["data.message_id", "data.status", "data.delivery", "data.reason"] };

export function recordView(event: SessionEvent): { open: ReadonlySet<string>; pinned: ReadonlySet<string> } {
  const rule = recordRules[event.kind];
  const data = event.data !== null && typeof event.data === "object" ? event.data as Record<string, JsonValue> | JsonValue[] : undefined;
  return { open: new Set(rule?.open ?? ["data"]), pinned: new Set(rule && data ? rule.pinned(data) : []) };
}

// The record a line opens: exactly what the events API returned for it.
export function eventRecord(event: SessionEvent): JsonValue {
  return { session_id: event.session_id, turn_id: event.turn_id, sequence: event.sequence, kind: event.kind, data: event.data, created_at: event.created_at };
}

export function payloadSize(event: SessionEvent): number | undefined {
  return event.data === null || (event.kind in words && Object.keys(event.data as object).length === 0) ? undefined : byteLength(JSON.stringify(event.data));
}

// Lowercased text an event matches against, computed once per event.
const searchable = new WeakMap<SessionEvent, string>();
export function searchText(event: SessionEvent): string {
  let text = searchable.get(event);
  if (text === undefined) {
    text = `${event.kind} #${event.sequence} ${eventSummary(event)} ${event.data === null ? "" : JSON.stringify(event.data)}`.toLowerCase();
    searchable.set(event, text);
  }
  return text;
}

// Blocks of the log in event order: a Turn's group sits where its first loaded
// event is and collects all of its loaded events; Session events stand alone.
export type LogBlock =
  | { kind: "turn"; sequence: number | undefined; turnID: string; events: SessionEvent[] }
  | { kind: "session"; event: SessionEvent };

export function buildBlocks(events: readonly SessionEvent[], turnSequence: (id: string) => number | undefined): LogBlock[] {
  const blocks: LogBlock[] = [];
  const groups = new Map<string, Extract<LogBlock, { kind: "turn" }>>();
  for (const event of events) {
    if (event.turn_id === null) {
      blocks.push({ kind: "session", event });
      continue;
    }
    let group = groups.get(event.turn_id);
    if (!group) {
      group = { kind: "turn", sequence: turnSequence(event.turn_id), turnID: event.turn_id, events: [] };
      groups.set(event.turn_id, group);
      blocks.push(group);
    }
    group.events.push(event);
  }
  return blocks;
}

export function inputSummary(turn: SessionTurn | undefined): string {
  if (!turn) return "";
  if (turn.payload_expired_at) return "Content expired";
  const value = turn.input;
  if (Array.isArray(value) && value.every(part => part !== null && typeof part === "object" && !Array.isArray(part) && part["type"] === "text" && typeof part["text"] === "string"))
    return firstLine(value.map(part => (part as { text: string }).text).join(""));
  return value === undefined ? "" : preview(value);
}

export function duration(from: string | undefined, to: string | undefined, now = Date.now()): string {
  if (!from) return "";
  const ms = Math.max(0, (to ? Date.parse(to) : now) - Date.parse(from));
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, "0")}m`;
}

export function clockTime(value: string): string {
  const date = new Date(value);
  const pad = (n: number, size = 2) => String(n).padStart(size, "0");
  return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}.${pad(date.getMilliseconds(), 3)}`;
}
