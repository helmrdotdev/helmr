import { createHash } from "node:crypto"
import { readFile, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import type { CommandRef } from "@helmr/sdk"
import { assert, assertEqual, deadline, errorCode, verify } from "../../support/context"
import { hostObservation } from "../../support/host-observation"

const endpointDirectory = process.env.HELMR_NETWORK_CLIENT_ENDPOINT_DIR
assert(endpointDirectory && process.env.HELMR_RUNTIME_HOST_TOOL,
  "Provide the running endpoint directory and installed HELMR_RUNTIME_HOST_TOOL; run on the dedicated host")
const runner = await readFile(new URL("../../support/network-clients.mjs", import.meta.url))
const configBytes = await readFile(join(endpointDirectory, "config.json"))
const ca = await readFile(join(endpointDirectory, "ca.pem"))
const config = JSON.parse(configBytes.toString())
const versions = { node: "v24.22.1", npm: "11.19.1", git: "git version 2.51.2", anthropic: "0.98.0" }
assertEqual(config.expectedVersions, versions, "Endpoint tools differ from the pinned client matrix")
assert(/^[a-f0-9]{64}$/.test(config.fixtureBinarySHA256), "Endpoint executable identity is missing")
const hash = (bytes: Uint8Array) => createHash("sha256").update(bytes).digest("hex")
const toolPath = "/opt/network-client-tools/node/bin:/opt/network-client-tools/git/bin:/usr/bin:/bin"
const guestDirectory = "/workspace/network-clients"

await verify("network-clients", async ({ computer, marker }) => {
  const evidence = process.env.HELMR_EVIDENCE_DIR!
  const record = (name: string, value: unknown) => writeFile(join(evidence, name),
    JSON.stringify(value, null, 2) + "\n", { mode: 0o600, flag: "wx" })
  const target = await computer("network-client-qualification")
  async function admitStage(request: Parameters<typeof target.exec>[0]) {
    // Computer creation admits asynchronous image preparation; exec creates demand.
    const signal = deadline(300_000)
    for (;;) {
      signal.throwIfAborted()
      try {
        return await target.exec(request, { signal })
      } catch (error) {
        if (errorCode(error) !== "computer_busy") throw error
      }
      await delay(500, undefined, { signal })
    }
  }
  async function hibernateWhenIdle(request: Parameters<typeof target.hibernate>[0]) {
    // A terminal Command outcome can precede physical process reconciliation.
    const signal = deadline(120_000)
    for (;;) {
      signal.throwIfAborted()
      try {
        return await target.hibernate(request, { signal })
      } catch (error) {
        if (errorCode(error) !== "computer_busy") throw error
      }
      await delay(500, undefined, { signal })
    }
  }
  async function command(label: string, ref: CommandRef) {
    const signal = deadline(5 * 60_000)
    const outcome = await ref.wait({ signal })
    const output = { stdout: "", stderr: "" }
    for await (const item of ref.logs.stream({}, { signal })) {
      assert(item.kind !== "gap", `Command ${ref.id} lost output`)
      output[item.stream] += Buffer.from(item.content).toString("utf8")
      assert(output.stdout.length + output.stderr.length <= 64 * 1024, "Client output exceeds its bound")
    }
    await record(`${label}-command.json`, { commandId: ref.id, outcome, ...output })
    assert(outcome.kind === "exited" && outcome.exitCode === 0, `Command ${ref.id} failed: ${JSON.stringify(outcome)} ${output.stderr}`)
    return output.stdout
  }
  const files = { "network-clients.mjs": runner, "config.json": configBytes, "ca.pem": ca }
  const stdin = Buffer.from(JSON.stringify(Object.fromEntries(Object.entries(files).map(([name, bytes]) => [name, bytes.toString("base64")]))))
  assert(stdin.length <= 1024 * 1024, "Staged fixture exceeds the native Computer stdin limit")
  const packaged = JSON.parse(await command("stage", await admitStage({
    command: ["node", "--input-type=module", "-e", `
      import { mkdir, readFile, writeFile } from "node:fs/promises";
      let input = ""; for await (const chunk of process.stdin) input += chunk;
      const files = JSON.parse(input);
      await mkdir("${guestDirectory}", { mode: 0o700 });
      for (const name of ["network-clients.mjs", "config.json", "ca.pem"])
        await writeFile("${guestDirectory}/" + name, Buffer.from(files[name], "base64"), { mode: 0o600, flag: "wx" });
      console.log(await readFile("/opt/network-client-tools/artifacts.json", "utf8"));
    `], env: { PATH: toolPath }, stdin, timeout: "2m", idempotencyKey: `${marker}:stage`,
  })))
  assertEqual(packaged.versions, versions, "Image tool manifest differs from the pinned matrix")

  async function probe(label: string) {
    const stdout = await command(label, await target.exec({
      command: ["node", `${guestDirectory}/network-clients.mjs`],
      env: { PATH: toolPath, NETWORK_CLIENT_CONFIG: `${guestDirectory}/config.json`,
        NETWORK_CLIENT_SDK_ROOT: "/opt/network-client-tools/sdk/node_modules/@anthropic-ai/sdk",
        NODE_EXTRA_CA_CERTS: `${guestDirectory}/ca.pem` },
      timeout: "2m", idempotencyKey: `${marker}:${label}`,
    }, { signal: deadline(5 * 60_000) }))
    const receipt = JSON.parse(stdout)
    assertEqual(receipt.syntheticFixture, true, "Client receipt is not synthetic fixture evidence")
    assertEqual(receipt.liveModelProvider, false, "Unexpected live-provider claim")
    assertEqual(receipt.versions, versions, "Native client versions changed")
    assertEqual(receipt.runnerSHA256, hash(runner), "Computer ran another client runner")
    assertEqual(receipt.caSHA256, hash(ca), "Computer used another fixture CA")
    assertEqual(receipt.fixtureBinarySHA256, config.fixtureBinarySHA256, "Computer reached another endpoint executable")
    for (const key of ["nativeModelStreaming", "gitHTTPS", "npmAuthenticatedRedirectChain", "deniedNpmRedirect", "deniedNumericTLS", "privateExplicitCA"])
      assertEqual(receipt.checks[key], true, `Native client check missing: ${key}`)
    assertEqual(receipt.report.deniedOriginContacts, 0, "Forbidden origin received client traffic")
    assertEqual(receipt.report.crossOriginCredentials, 0, "Registry credentials crossed origins")
    return receipt
  }
  const before = await probe("before")
  assertEqual(before.sdkPackageSHA256, packaged.sdkPackageSHA256, "Computer used another packaged SDK")
  const source = await hostObservation("computer-path", { computer_id: target.id })
  assertEqual(source.computer.id, target.id, "Source observation belongs to another Computer")
  // A published image can supply the root without a per-Computer build receipt.
  assert(typeof source.computer.initial_root_id === "string" && source.computer.initial_root_id.length > 0 && /^[a-f0-9]{64}$/.test(source.computer.initial_root_digest), "Source omitted immutable seed identity")
  const leases = source.leases.filter((lease: any) => lease.status === "active" && lease.fenced_at === null)
  assertEqual(leases.length, 1, "Expected one active source lease")
  const lease = leases[0]
  await record("source.json", source)
  await hibernateWhenIdle({ idempotencyKey: `${marker}:hibernate` })
  const captured = await waitPath(path => path.checkpoints.some((checkpoint: any) => checkpoint.source_lease_epoch === lease.epoch && checkpoint.status === "ready")
    && path.leases.some((old: any) => old.epoch === lease.epoch && old.status === "lost" && old.fenced_at !== null))
  const checkpoint = captured.checkpoints.find((item: any) => item.source_lease_epoch === lease.epoch && item.status === "ready")
  assert(checkpoint.disk_save_id && checkpoint.target_lease_epoch === null, "Checkpoint was not durable before Wake")
  await record("captured.json", captured)
  await target.wake({ idempotencyKey: `${marker}:wake` })
  const restored = await waitPath(path => path.checkpoints.some((item: any) => item.id === checkpoint.id && item.status === "consumed")
    && path.leases.some((item: any) => item.epoch > lease.epoch && item.status === "active" && item.fenced_at === null && item.initialized_at !== null))
  const consumed = restored.checkpoints.find((item: any) => item.id === checkpoint.id)
  const destination = restored.leases.find((item: any) => item.epoch === consumed.target_lease_epoch)
  assert(destination && destination.status === "active" && destination.fenced_at === null, "Checkpoint did not select the active destination")
  assertEqual(destination.restored_from_save_id, checkpoint.disk_save_id, "Restore selected another disk save")
  assert.notEqual(destination.computer_instance_id, lease.computer_instance_id, "Restore reused the original Instance")
  assertEqual(destination.worker_host_id, lease.worker_host_id, "Same-host restore selected another Host")
  assert(consumed.controls_reconciled_at !== null, "Restored controls were not reconciled")
  for (const key of ["id", "image_build_id", "initial_root_id", "initial_root_digest"])
    assertEqual(restored.computer[key], source.computer[key], `Restore changed Computer ${key}`)
  await record("restored.json", restored)
  const after = await probe("after")
  for (const key of ["versions", "fixtureBinarySHA256", "sdkPackageSHA256", "caSHA256", "runnerSHA256"])
    assertEqual(after[key], before[key], `Restore changed native client artifact ${key}`)
  return { computerId: target.id, before, after, physicalCrossHost: false }

  async function waitPath(matches: (path: any) => boolean) {
    const signal = deadline(10 * 60_000)
    for (;;) {
      const path = await hostObservation("computer-path", { computer_id: target.id })
      if (matches(path)) return path
      await delay(500, undefined, { signal })
    }
  }
})
