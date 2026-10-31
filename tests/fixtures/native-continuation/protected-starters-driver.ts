import assert from "node:assert/strict"
import { readFile, writeFile, rename } from "node:fs/promises"
import { setTimeout as delay } from "node:timers/promises"
import type { HelmrClient } from "../../../sdk/typescript/src/client"
import type { ComputerRef, ComputerCommandRequest } from "../../../sdk/typescript/src/computer"
import { isHelmrError } from "../../../sdk/typescript/src/error"
import type { SessionRef, TurnRef } from "../../../sdk/typescript/src/contract"
import { fixtureInput } from "../../e2e/support/runtime-mcp"

type Observation = { unauthenticatedPreconnects: number; operations: number[]; versions: number[]; requests: unknown[] | null; handshakes: number; closed: number; http: number; activeHTTP: number; unauthorized: number; fallback: number }
const kinds = ["claude", "codex", "opencode-anthropic", "opencode-openai", "opencode-go", "pi-anthropic", "pi-openai", "pi-go"] as const

async function execPrepared(computer: ComputerRef, request: ComputerCommandRequest, signal: AbortSignal) {
  for (;;) {
    try { return await computer.exec(request, { signal }) }
    catch (error) {
      if (!isHelmrError(error) || error.code !== "computer_busy") throw error
      await delay(100, undefined, { signal })
    }
  }
}

async function deleteFixtureComputer(computer: ComputerRef, idempotencyKey: string) {
  // Storage is released by the ordinary retention loop after physical cleanup.
  const signal = AbortSignal.timeout(180_000)
  for (;;) {
    try { await computer.delete({ idempotencyKey }, { signal }); break }
    catch (error) {
      if (!isHelmrError(error) || error.code !== "computer_busy") throw error
      await delay(100, undefined, { signal })
    }
  }
  while ((await computer.get({ signal })).status !== "deleted") await delay(100, undefined, { signal })
}

export async function runProtectedStarters(client: HelmrClient, fixture: { origin: string; state: string }, evidence: string, signal: AbortSignal) {
  const records: Record<string, unknown>[] = []
  for (const kind of kinds) {
    const startedAt = Date.now()
    let primaryError: unknown
    let session: SessionRef | undefined
    let newerSession: SessionRef | undefined
    let computer: ComputerRef | undefined
    const name = `protected-${kind}`
    await client.secrets.create({ secret: name, value: "synthetic-protected-native-key", idempotencyKey: name }, { signal })
    try {
      computer = await client.computers.create({ template: "native-continuation", idempotencyKey: name,
        secrets: [{ secret: name, env: { name: "NATIVE_PROTECTED_KEY", mode: "protected", allowedOrigins: [fixture.origin] } }],
      }, { signal })
      const commands = client.computers.ref(computer.id)
      async function command(script: string, phase: string) {
        const command = await execPrepared(commands, { command: ["/usr/local/bin/node", "--input-type=module", "-e", script], timeout: "60s", idempotencyKey: `${name}-${phase}` }, signal)
        const result = await command.wait({ signal })
        assert.equal(result.kind, "exited"); assert.equal(result.exitCode, 0, phase)
      }
      await command(`import {writeFile} from 'node:fs/promises'; await writeFile('/workspace/protected-fixture.json', ${JSON.stringify(JSON.stringify({ origin: fixture.origin }))});`, "configure")
      const started = await client.sessions.create({ agent: name, computer, input: fixtureInput("protected remembered first") }, { signal })
      session = started.session
      async function completed(turn: TurnRef) {
        const outcome = await turn.wait({ signal })
        assert.equal(outcome.status, "completed", JSON.stringify(outcome))
        const result = outcome.result as Record<string, unknown>
        assert.equal(result.selectorOnly, true)
        return result
      }
      async function observed(predicate: (value: Observation) => boolean) {
        for (;;) {
          const state = JSON.parse(await readFile(fixture.state, "utf8")) as Record<string, Observation>
          const value = state[session!.id]
          if (value && predicate(value)) return value
          await delay(100, undefined, { signal })
        }
      }
      async function hold(enabled: boolean) {
        await writeFile(fixture.state + ".hold.tmp", JSON.stringify({ [session!.id]: enabled }), { mode: 0o600 })
        await rename(fixture.state + ".hold.tmp", fixture.state + ".hold")
      }
      const first = await completed(started.turn)
      await client.secrets.ref(name).rotate({ value: "synthetic-protected-native-key-rotated", idempotencyKey: `${name}-rotate` }, { signal })
      const second = await completed(await session.enqueue(fixtureInput("protected retained second"), undefined, { signal }))
      assert.equal(second.nonce, first.nonce)
      const identity = kind === "codex" ? "nativeThreadId" : "nativeSessionId"
      assert.equal(typeof first[identity], "string")
      assert.equal(second[identity], first[identity])
      const retained = await observed(value => (value.requests?.length ?? 0) >= 2)
      assert(JSON.stringify(retained.requests).includes("protected remembered first"))
      assert.equal(retained.unauthorized, retained.unauthenticatedPreconnects); assert.equal(retained.fallback, 0)
      if (kind === "codex" || kind === "opencode-openai") {
        assert(retained.handshakes > 0, "required WebSocket transport was unused")
      }
      await hold(true)
      const beforeInterrupt = retained.requests!.length
      const interrupted = await session.enqueue(fixtureInput("protected interrupt"), undefined, { signal })
      await observed(value => (value.requests?.length ?? 0) > beforeInterrupt && JSON.stringify(value.requests?.at(-1)).includes("protected interrupt") && (value.requests?.at(-1) as { generate?: boolean })?.generate !== false)
      await interrupted.interrupt(undefined, { signal })
      assert.equal((await interrupted.wait({ signal })).status, "interrupted")
      await hold(false)
      const afterInterrupt = await completed(await session.enqueue(fixtureInput("protected after interrupt"), undefined, { signal }))
      assert.equal(afterInterrupt.nonce, first.nonce)
      assert.equal(afterInterrupt[identity], first[identity])
      const beforeLoss = await observed(() => true)
      await hold(true)
      const lost = await session.enqueue(fixtureInput("protected owner loss"), undefined, { signal })
      const lossState = await observed(value => (value.requests?.length ?? 0) > beforeLoss.requests!.length && JSON.stringify(value.requests?.at(-1)).includes("protected owner loss") && (value.requests?.at(-1) as { generate?: boolean })?.generate !== false)
      assert(lossState.versions.length > 0 && lossState.versions.every(version => version === 1), "rotation changed the admitted process's pinned version")
      await command(`import {readFile} from 'node:fs/promises'; const {port}=JSON.parse(await readFile(${JSON.stringify(`/workspace/protected-${kind}/${session.id}/control.json`)},'utf8')); const response=await fetch('http://127.0.0.1:'+port+'/crash',{method:'POST'}); if(!response.ok)throw new Error('owner crash refused'); await response.text();`, "owner-loss")
      assert.equal((await lost.wait({ signal })).status, "interrupted")
      if (beforeLoss.handshakes > 0) await observed(value => value.closed === value.handshakes)
      await hold(false)
      // One owner loss is one process failure; the fresh input reconstructs
      // the process without a Session hold.
      const fresh = await session.enqueue(fixtureInput("protected reconstructed fresh"), undefined, { signal })
      const reconstructed = await completed(fresh)
      assert.deepEqual((await session.get({ signal })).holds, [])
      assert.notEqual(reconstructed.nonce, first.nonce)
      assert.equal(reconstructed[identity], first[identity])
      const recovered = await observed(value => JSON.stringify(value.requests).includes("protected reconstructed fresh"))
      assert(JSON.stringify(recovered.requests!.at(-1)).includes("protected remembered first"), "native history was lost")
      const firstCurrentVersion = recovered.versions.indexOf(2)
      assert(firstCurrentVersion >= lossState.versions.length && recovered.versions.slice(firstCurrentVersion).every(version => version === 2), "reconstructed process did not select the current version")
      const newer = await client.sessions.create({ agent: name, computer, input: fixtureInput("protected new owner after rotation") }, { signal })
      newerSession = newer.session
      await completed(newer.turn)
      const allOwners = JSON.parse(await readFile(fixture.state, "utf8")) as Record<string, Observation>
      const newOwner = allOwners[newer.session.id]!
      assert(newOwner.versions.length > 0 && newOwner.versions.every(version => version === 2), "new owner did not receive the current version")
      await newer.session.cancel(undefined, { signal })
      await hold(true)
      const finalTurn = await session.enqueue(fixtureInput("protected revoke active"), undefined, { signal })
      await observed(value => (value.requests?.length ?? 0) > recovered.requests!.length && JSON.stringify(value.requests?.at(-1)).includes("protected revoke active") && (value.requests?.at(-1) as { generate?: boolean })?.generate !== false)
      await client.secrets.ref(name).revoke({ idempotencyKey: `${name}-revoke` }, { signal })
      const revoked = await finalTurn.wait({ signal })
      assert.notEqual(revoked.status, "completed")
      const finalState = await observed(value => value.handshakes === value.closed && value.activeHTTP === 0)
      assert.equal(finalState.unauthorized, finalState.unauthenticatedPreconnects); assert.equal(finalState.fallback, 0)
      records.push({ kind, durationMs: Date.now() - startedAt, sessionId: session.id, first, second, afterInterrupt, reconstructed, newOwner, revoked, observation: finalState })
      await writeFile(`${evidence}/protected-starters.json`, JSON.stringify(records) + "\n", { mode: 0o600 })
    } catch (error) {
      primaryError = error
      throw error
    } finally {
      const cleanup = AbortSignal.timeout(30_000)
      const results = await Promise.allSettled([
        ...[session, newerSession].filter((value): value is SessionRef => !!value).map(async value => {
          const state = await value.get({ signal: cleanup })
          if (state.status !== "closed" && state.status !== "cancelled") await value.cancel(undefined, { signal: cleanup })
        }),
        client.secrets.ref(name).revoke({ idempotencyKey: `${name}-revoke` }, { signal: cleanup }),
      ])
      if (computer) results.push(...await Promise.allSettled([deleteFixtureComputer(computer, `${name}-delete`)]))
      const errors = results.filter(result => result.status === "rejected")
      if (errors.length) throw new AggregateError([...(primaryError ? [primaryError] : []), ...errors.map(result => result.reason)], "protected fixture cleanup failed")
    }
  }
  const authority = await runProtectedAuthority(client, fixture, signal)
  await writeFile(`${evidence}/protected-authority.json`, JSON.stringify(authority) + "\n", { mode: 0o600 })
  return records
}

// Separate Command and lease owners exercise shutdown paths shared by the native
// pairs. The client performs an ordinary authenticated Upgrade and keeps it open.
async function runProtectedAuthority(client: HelmrClient, fixture: { origin: string; state: string }, signal: AbortSignal) {
  const name = "protected-authority"
  await client.secrets.create({ secret: name, value: "synthetic-protected-native-key", idempotencyKey: name }, { signal })
  const commands: import("../../../sdk/typescript/src/command").CommandRef[] = []
  let primaryError: unknown
  let computer: ComputerRef | undefined
  try {
    computer = await client.computers.create({ template: "native-continuation", idempotencyKey: name,
      secrets: [{ secret: name, env: { name: "NATIVE_PROTECTED_KEY", mode: "protected", allowedOrigins: [fixture.origin] } }],
    }, { signal })
    const computerRef = computer
    async function socket(owner: string) {
      const command = await execPrepared(computerRef, { command: ["/usr/local/bin/node", "--input-type=module", "-e", `
        import https from 'node:https'; import {randomBytes} from 'node:crypto';
        const request=https.request(${JSON.stringify(fixture.origin)}+'/command/'+${JSON.stringify(owner)}+'/v1/responses', {headers:{
          Authorization:'Bearer '+process.env.NATIVE_PROTECTED_KEY, Connection:'Upgrade', Upgrade:'websocket',
          'Sec-WebSocket-Version':'13','Sec-WebSocket-Key':randomBytes(16).toString('base64')
        }});
        request.on('upgrade', (response,socket)=>{socket.on('error',()=>{});socket.resume()});
        request.on('response',response=>{throw new Error('upgrade refused '+response.statusCode)});
        request.on('error',error=>{throw error});request.end();
      `], timeout: "5m", idempotencyKey: owner }, signal)
      commands.push(command)
      return command
    }
    async function observed(owner: string, predicate: (value: Observation & { deniedRenewals: number }) => boolean) {
      for (;;) {
        const all = JSON.parse(await readFile(fixture.state, "utf8"))
        if (all[owner] && predicate(all[owner])) return all[owner]
        await delay(100, undefined, { signal })
      }
    }
    const cancelled = await socket("command-cancel")
    await Promise.race([observed("command-cancel", value => value.handshakes === 1), cancelled.wait({ signal }).then(outcome => { throw new Error(`Command ended before Upgrade: ${JSON.stringify(outcome)}`) })])
    await cancelled.cancel({ signal })
    const cancelledOutcome = await cancelled.wait({ signal })
    assert.equal(cancelledOutcome.kind, "cancelled")
    const commandState = await observed("command-cancel", value => value.closed === 1)
    const expired = await socket("lease-expiry")
    await Promise.race([observed("lease-expiry", value => value.handshakes === 1), expired.wait({ signal }).then(outcome => { throw new Error(`Command ended before Upgrade: ${JSON.stringify(outcome)}`) })])
    const faultStartedAt = Date.now()
    await writeFile(fixture.state + ".lease-outage.tmp", computer.id, { mode: 0o600 })
    await rename(fixture.state + ".lease-outage.tmp", fixture.state + ".lease-outage")
    const leaseState = await observed("lease-expiry", value => value.deniedRenewals > 0 && value.closed === 1)
    const leaseClosureMs = Date.now() - faultStartedAt
    await writeFile(fixture.state + ".lease-outage", "", { mode: 0o600 })
    const expiredOutcome = await expired.wait({ signal })
    assert.equal(expiredOutcome.kind, "system_failed", "lease loss was not reflected as execution loss")
    for (;;) {
      const current = await expired.get({ signal })
      assert.equal(current.status, "lost")
      if (current.processReconciled) break
      await delay(100, undefined, { signal })
    }
    return { computerId: computer.id, cancelledOutcome, commandState, expiredOutcome, leaseState, leaseClosureMs }
  } catch (error) {
    primaryError = error
    throw error
  } finally {
    await writeFile(fixture.state + ".lease-outage", "", { mode: 0o600 })
    const cleanup = AbortSignal.timeout(30_000)
    const results = await Promise.allSettled([
      ...commands.map(async command => {
        const current = await command.get({ signal: cleanup })
        if (["pending", "starting", "running", "stopping"].includes(current.status)) {
          await command.cancel({ signal: cleanup }); await command.wait({ signal: cleanup })
        }
      }),
      client.secrets.ref(name).revoke({ idempotencyKey: `${name}-revoke` }, { signal: cleanup }),
    ])
    if (computer) results.push(...await Promise.allSettled([deleteFixtureComputer(computer, `${name}-delete`)]))
    const errors = results.filter(result => result.status === "rejected")
    if (errors.length) throw new AggregateError([...(primaryError ? [primaryError] : []), ...errors.map(result => result.reason)], "protected authority cleanup failed")
  }
}
