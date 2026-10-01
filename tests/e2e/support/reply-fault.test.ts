import { test, expect } from "bun:test"
import { assertReplyLosses, type ReplyFault, type ReplyEvent } from "./reply-fault"

const healthy = "healthy", cancelled = "cancelled"
function evidence(): ReplyFault {
  const event = (kind: string, dropped: boolean, disposition?: string): ReplyEvent => ({
    kind, dropped, disposition, at: "2026-01-01T00:00:00Z", abort_desired_version: 3,
    members: disposition === "aborted" ? [
      { run_id: healthy, cancelled: false, expires_at: "2026-01-01T00:01:00Z" },
      { run_id: cancelled, cancelled: true, expires_at: "2025-12-31T23:59:00Z" },
    ] : undefined,
  })
  return { stage: 4, failure: "", target: null, relay: { restored: true, active_streams: 0, forwarded_streams: 10 }, events: [
    event("cp-abort", true, "aborted"), event("cp-abort", false, "aborted"),
    event("guest-prepare", true), event("cp-abort", false, "aborted"), event("guest-prepare", false),
    event("guest-activate", true), event("cp-abort", false, "aborted"), event("guest-prepare", false),
    event("guest-activate", false), event("cp-complete", true), event("cp-abort", false, "acknowledged"),
  ] }
}

test("reply proof accepts current grants even when rapid retries share a lease expiry", () => {
  expect(() => assertReplyLosses(evidence(), healthy, cancelled)).not.toThrow()
})

test("reply proof rejects missing loss, replay, cancellation, authority and socket restoration", () => {
  const mutations: ((state: ReplyFault) => void)[] = [
    s => { s.events = s.events!.filter(e => e.kind !== "cp-complete") },
    s => { s.events = s.events!.filter(e => e.kind !== "guest-activate" || e.dropped) },
    s => { for (const e of s.events!) if (e.kind === "guest-activate" && !e.dropped) e.error = "complete response not received" },
    s => { s.events = s.events!.filter(e => e.disposition !== "acknowledged") },
    s => { s.events![0]!.members![1]!.cancelled = false },
    s => { s.events![0]!.members![0]!.expires_at = "2025-12-31T23:59:00Z" },
    s => { s.events![1]!.abort_desired_version = 4 },
    s => { s.relay!.restored = false },
    s => { s.failure = "upstream failed before applying" },
  ]
  for (const mutate of mutations) {
    const state = evidence()
    mutate(state)
    expect(() => assertReplyLosses(state, healthy, cancelled)).toThrow()
  }
})
