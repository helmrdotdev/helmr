import { assert, deadline } from "./context"

export type ReplyEvent = {
  kind: string; at: string; dropped: boolean; desired_version?: number; digest?: string
  installed?: boolean; activated?: boolean; stopped_session_ids?: string[]; expires_at?: string
}
export type ReplyFault = {
  stage: number; failure: string; released: boolean; events: ReplyEvent[] | null
  target: { checkpoint_id?: string; instance_id: string; computer_id: string; writer_generation: number;
    members?: { session_id: string; process_epoch: number }[] } | null
  relay?: { restored: boolean; active_streams: number; forwarded_streams: number; stream_errors?: number }
}

export async function replyFault(method = "GET", body?: unknown): Promise<ReplyFault> {
  const response = await fetch("http://127.0.0.1:58089/__replies", {
    method, headers: { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body), signal: deadline(15_000),
  })
  assert(response.ok, `Reply fault returned ${response.status}: ${await response.clone().text()}`)
  return response.json()
}

export function assertReplyLosses(state: ReplyFault, healthySession: string, cancelledSession: string) {
  assert.equal(state.failure, "")
  assert.equal(state.stage, 5)
  assert.equal(state.released, true)
  assert.equal(state.relay?.restored, true, "source socket pathname was not restored")
  assert.deepEqual(state.target?.members?.map(m => m.session_id).sort(), [healthySession, cancelledSession].sort())
  const events = state.events ?? []
  assert.deepEqual(events.filter(e => e.dropped).map(e => e.kind),
    ["guest-capture", "cp-prepare", "guest-install", "guest-activate", "cp-complete"])
  for (const kind of ["cp-prepare", "guest-install", "guest-activate", "cp-complete"]) {
    const lost = events.findIndex(e => e.kind === kind && e.dropped)
    assert(events.slice(lost + 1).some(e => e.kind === kind && !e.dropped), `${kind} was not replayed`)
  }
  const installed = events.findIndex(e => e.kind === "guest-install" && e.dropped)
  const identity = events[installed]!.digest
  assert(identity)
  for (const event of events.slice(installed).filter(e => ["cp-prepare", "guest-install", "guest-activate", "cp-complete"].includes(e.kind))) {
    assert.equal(event.digest, identity, "installed authority changed during reconciliation")
    assert.equal(event.desired_version, events[installed]!.desired_version)
  }
  const controls = events.filter(e => e.kind === "guest-controls")
  assert(controls.length > 0, "activation omitted current controls")
  for (const control of controls) assert.deepEqual(control.stopped_session_ids, [cancelledSession])
}
