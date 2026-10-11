import { test, expect } from "bun:test"
import { assertReplyLosses, type ReplyFault, type ReplyEvent } from "./reply-fault"

const healthy = "healthy", cancelled = "cancelled"
function evidence(): ReplyFault {
  const event = (kind: string, dropped = false): ReplyEvent => ({
    kind, dropped, at: "2026-01-01T00:00:00Z", desired_version: 3, digest: "installed-digest",
    ...(kind === "guest-controls" ? { stopped_session_ids: [cancelled] } : {}),
  })
  return { stage: 5, failure: "", released: true,
    target: { computer_id: "computer", instance_id: "instance", writer_generation: 1,
      members: [{ session_id: healthy, process_epoch: 1 }, { session_id: cancelled, process_epoch: 1 }] },
    relay: { restored: true, active_streams: 0, forwarded_streams: 10 }, events: [
      event("guest-capture", true), event("cp-prepare", true), event("cp-prepare"),
      event("guest-install", true), event("guest-inspect"), event("guest-install"),
      event("guest-controls"), event("guest-activate", true), event("guest-install"),
      event("guest-activate"), event("cp-complete", true), event("cp-complete"),
    ] }
}

test("reply proof accepts immutable installation replay and current cancellation controls", () => {
  expect(() => assertReplyLosses(evidence(), healthy, cancelled)).not.toThrow()
})

test("reply proof rejects missing loss, replay, cancellation, authority and socket restoration", () => {
  const mutations: ((state: ReplyFault) => void)[] = [
    s => { s.events = s.events!.filter(e => e.kind !== "guest-capture") },
    s => { s.events = s.events!.filter(e => e.kind !== "cp-complete" || e.dropped) },
    s => { s.events = s.events!.filter(e => e.kind !== "guest-activate" || e.dropped) },
    s => { s.events!.find(e => e.kind === "guest-controls")!.stopped_session_ids = [] },
    s => { s.events!.find(e => e.kind === "guest-activate")!.digest = "different" },
    s => { s.events!.find(e => e.kind === "cp-complete")!.desired_version = 4 },
    s => { s.target!.members!.pop() },
    s => { s.released = false },
    s => { s.relay!.restored = false },
    s => { s.failure = "upstream failed before applying" },
  ]
  for (const mutate of mutations) {
    const state = evidence()
    mutate(state)
    expect(() => assertReplyLosses(state, healthy, cancelled)).toThrow()
  }
})
