import { expect, test } from "bun:test";
import type { SessionEvent } from "../../lib/sessions";
import { buildBlocks, duration, eventSummary, isOutput, isProblem } from "./log";

const event = (sequence: number, kind: string, turn: string | null, data: SessionEvent["data"] = {}): SessionEvent =>
  ({ session_id: "s", turn_id: turn, sequence, kind, data, created_at: "2026-10-15T10:00:00Z" });

test("groups a Turn's events where its first event is and keeps Session events in place", () => {
  const blocks = buildBlocks([
    event(1, "turn.queued", "a"), event(2, "turn.running", "a"), event(3, "turn.queued", "b"),
    event(4, "turn.completed", "a"), event(5, "session.close", null, { reason: "close_after_idle" }), event(6, "turn.running", "b"),
  ], id => ({ a: 1, b: 2 })[id]);
  expect(blocks.map(block => block.kind === "turn" ? `${block.sequence}:${block.events.map(e => e.sequence).join(",")}` : `s${block.event.sequence}`))
    .toEqual(["1:1,2,4", "2:3,6", "s5"]);
});

test("summarises every event kind without its kind", () => {
  expect(eventSummary(event(1, "turn.output", "a", [{ type: "text", text: "Running 214 tests\nmore" }, { type: "json", value: { shard: 3 } }]))).toBe('"Running 214 tests" · json {shard: 3}');
  expect(eventSummary(event(2, "turn.failed", "a", { code: "handler_error" }))).toBe("handler_error");
  expect(eventSummary(event(3, "slack.delivery_unavailable", null, { reason: "route_unavailable", source_seq: 12 }))).toBe("Slack update for #12 not posted · route_unavailable");
  expect(eventSummary(event(4, "ask.created", "a", { ask_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", question: { prompt: [{ type: "text", text: "Merge?" }], title: "Merge PR #8812?", type: "choice", options: [{ id: "merge", label: "Merge now", value: true }, { id: "wait", label: "Wait", value: false }] } })))
    .toBe("Merge PR #8812? · choice: Merge now / Wait");
  expect(eventSummary(event(5, "message.rejected", "a", { message_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33", status: "rejected", delivery: "uncertain" }))).toContain("delivery uncertain");
  expect(eventSummary(event(6, "session.close", null, { reason: "close_after_idle" }))).toBe("closing after idle");
  expect(eventSummary(event(7, "future.kind", null, { anything: 1 }))).toBe("{anything: 1}");
  expect(eventSummary(event(8, "turn.output", "a", null))).toBe("content expired");
  const id = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33";
  expect(eventSummary(event(9, "turn.interrupted", "a", { reason: "turn_deadline_elapsed" }))).toBe("stopped after the maximum turn duration; queued work continues");
  expect(eventSummary(event(10, "session.process_failed", null, { processEpoch: 2, reason: "process_failed", holdId: null }))).toBe("process 2 failed · process_failed; queued work continues");
  expect(eventSummary(event(11, "session.process_failed", null, { processEpoch: 3, reason: "execution_lost", holdId: id }))).toBe("process 3 failed · execution_lost; work is held");
  expect(eventSummary(event(12, "session.recovery_gap", null, { holdId: null, computerId: id, leaseEpoch: 1, recoverySaveId: null, restoredAt: null, affectedTurnIds: [] }))).toContain("No completed requests in this session were affected");
});

test("classifies kinds into output and problems", () => {
  expect(["turn.output", "ask.created", "ask.answered"].every(isOutput) && !isOutput("turn.completed") && !isOutput("message.started")).toBe(true);
  expect(isProblem("turn.failed") && isProblem("message.rejected") && !isProblem("turn.completed")).toBe(true);
});

test("formats durations compactly", () => {
  expect(duration("2026-10-15T10:00:00Z", "2026-10-15T10:00:42Z")).toBe("42s");
  expect(duration("2026-10-15T10:00:00Z", "2026-10-15T10:12:18Z")).toBe("12m 18s");
  expect(duration("2026-10-15T10:00:00Z", "2026-10-15T12:14:00Z")).toBe("2h 14m");
});
