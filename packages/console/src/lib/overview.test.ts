import "./browser-test-env";
import { describe, expect, test } from "bun:test";
import type { AttentionItem } from "./attention";
import type { ComputerListItem } from "./computers";
import { failureRate, orderComputers, overviewIssues, type OverviewFailure } from "./overview";

const now = Date.parse("2026-10-25T12:00:00Z");
const failure = (overrides: Partial<OverviewFailure> = {}): OverviewFailure => ({
  agent: "reporter", error_code: "tool_error", count: 9, last_hour: 4, first_at: "2026-10-25T11:10:00Z", last_at: "2026-10-25T11:58:00Z",
  last_session_id: "s1", last_turn_id: "t1", last_turn_sequence: 12, ...overrides,
});
const computer = (id: string, overrides: Partial<ComputerListItem> = {}): ComputerListItem => ({
  id, template: "base", deployment_id: "d", status: "available", execution_control: null, execution_wait: null, residency: "running",
  created_at: "2026-10-24T00:00:00Z", updated_at: "2026-10-25T11:00:00Z", ...overrides,
});
const item = (kind: AttentionItem["kind"], at: string, overrides: Partial<AttentionItem> = {}): AttentionItem => ({ kind, id: `${kind}-${at}`, at, agent: "triage", session_id: "s2", ...overrides });

describe("overviewIssues", () => {
  test("is empty when nothing is wrong", () => {
    expect(overviewIssues({ now, failures: [failure({ last_hour: 0 })], computers: [computer("c1")], attention: [item("question", "2026-10-25T11:30:00Z")] })).toEqual([]);
  });

  test("groups each cause into one issue, most urgent first", () => {
    const issues = overviewIssues({
      now,
      failures: [failure()],
      computers: [computer("c1", { execution_wait: { reason: "environment_limit" } }), computer("c2", { execution_wait: { reason: "scheduling" } }), computer("c3", { residency: "unavailable" }), computer("c4", { status: "deleted", residency: "unavailable" })],
      attention: [
        item("hold", "2026-10-25T08:00:00Z", { hold_kind: "process_failed" }),
        item("slack_reauthorization", "2026-10-25T09:00:00Z", { workspace_name: "Acme" }),
        item("question", "2026-10-25T09:00:00Z"),
        item("question", "2026-10-25T09:30:00Z", { agent: "agent" }),
        item("question", "2026-10-25T11:30:00Z"),
      ],
    });
    expect(issues.map((issue) => [issue.tone, issue.title])).toEqual([
      ["danger", "reporter is failing · 4 turns in the last hour"],
      ["warning", "2 computers are waiting for capacity"],
      ["warning", "1 computer is unavailable"],
      ["warning", "A session is held"],
      ["warning", "Slack connection needs reauthorization"],
      ["neutral", "2 questions unanswered for over 2 hours"],
    ]);
    expect(issues[0]!.target).toEqual({ kind: "turn", sessionID: "s1", turnID: "t1" });
    expect(issues[1]!.detail).toContain("computer limit");
    expect(issues[3]!.detail).toBe("triage · process failed repeatedly");
    expect(issues[5]!.detail).toBe("Asked by triage, agent");
  });

  test("adds nothing for parts the caller cannot read", () => {
    expect(overviewIssues({ now })).toEqual([]);
  });
});

test("failureRate rounds to one decimal and is undefined without turns", () => {
  expect(failureRate({ total: 147, failed: 11 })).toBe(7.5);
  expect(failureRate({ total: 0, failed: 0 })).toBeUndefined();
});

test("orderComputers puts problems first, then awake, newest first", () => {
  const ordered = orderComputers([
    computer("asleep", { residency: "parked", created_at: "2026-10-25T11:59:00Z" }),
    computer("awake-old", { created_at: "2026-10-25T09:00:00Z" }),
    computer("awake-new", { created_at: "2026-10-25T11:00:00Z" }),
    computer("waiting", { residency: "cold", execution_wait: { reason: "environment_limit" } }),
    computer("broken", { residency: "unavailable" }),
    computer("gone", { status: "deleted" }),
  ]);
  expect(ordered.map((c) => c.id)).toEqual(["broken", "waiting", "awake-new", "awake-old", "asleep"]);
});
