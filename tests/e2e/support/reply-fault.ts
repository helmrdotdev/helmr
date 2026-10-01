import { assert, deadline } from "./context"

export type ReplyEvent = {
  kind: string; at: string; dropped: boolean; error?: string; disposition?: string; abort_desired_version?: number
  members?: { run_id: string; cancelled: boolean; expires_at: string }[]
}
export type ReplyFault = {
  stage: number; failure: string; events: ReplyEvent[] | null
  target: { checkpoint_id: string; instance_id: string; computer_id: string } | null
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

export function assertReplyLosses(state: ReplyFault, healthyRun: string, cancelledRun: string) {
  assert.equal(state.failure, "")
  assert.equal(state.stage, 4)
  assert.equal(state.relay?.restored, true, "source socket pathname was not restored")
  const events = state.events ?? []
  assert.deepEqual(events.filter(e => e.dropped).map(e => e.kind),
    ["cp-abort", "guest-prepare", "guest-activate", "cp-complete"])
  for (const kind of ["guest-prepare", "guest-activate"]) {
    const lost = events.findIndex(e => e.kind === kind && e.dropped)
    assert(events.slice(lost + 1).some(e => e.kind === kind && !e.dropped && !e.error), `${kind} was not replayed`)
  }
  const grants = events.filter(e => e.kind === "cp-abort" && e.disposition === "aborted")
  assert(grants.length >= 4, "missing duplicate abort requests")
  const version = grants[0]!.abort_desired_version
  for (const grant of grants) {
    assert.equal(grant.abort_desired_version, version)
    assert.deepEqual(grant.members?.map(m => m.run_id).sort(), [healthyRun, cancelledRun].sort())
    assert.equal(grant.members?.find(m => m.run_id === cancelledRun)?.cancelled, true)
    const healthy = grant.members?.find(m => m.run_id === healthyRun)
    assert(healthy && !healthy.cancelled)
    // Replays must carry current, unexpired DB authority. Rapid retries can
    // legitimately see the same lease expiry before the next renewal tick.
    assert(Date.parse(healthy.expires_at) > Date.parse(grant.at), "replayed expired grant")
  }
  const complete = events.findIndex(e => e.kind === "cp-complete" && e.dropped)
  assert(events.slice(complete + 1).some(e => e.kind === "cp-abort" && e.disposition === "acknowledged" && !e.dropped),
    "lost completion was not reconciled through the durable abort receipt")
}
