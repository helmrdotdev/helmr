import { expect, test } from "bun:test"
import { configuration } from "./config"
import { positiveTargets, requireConfigured, validatePositiveReceipt, type PositiveRequest } from "./positive-control"

const now = Date.parse("2026-10-27T12:00:00.000Z")
const uuid = (digit: string) => `${digit.repeat(8)}-${digit.repeat(4)}-4${digit.repeat(3)}-8${digit.repeat(3)}-${digit.repeat(12)}`
const request: PositiveRequest = {
  marker: uuid("1"), phase: "first", configurationDigest: "a".repeat(64), computerId: uuid("2"), turnId: uuid("3"),
  workerHostId: uuid("4"), requestedAt: new Date(now - 30_000).toISOString(),
  targets: positiveTargets(configuration, "finite"), ipv6: "guest-disabled-topology",
}
const receipt = () => ({ ...structuredClone(request), observedAt: new Date(now - 1000).toISOString(), evidencePath: "/private/actual-host-evidence.json",
  placement: { evidencePath: "/private/native-computer-path/result.json", observation: {
    observed_at: new Date(now - 2000).toISOString(), computer: { id: request.computerId },
    leases: [{ epoch: 1, worker_epoch: 1, computer_instance_id: uuid("5"), worker_host_id: request.workerHostId,
      status: "active", fenced_at: null as string | null, expires_at: new Date(now + 60_000).toISOString() }],
  } },
  results: request.targets.map(target => ({ target: { ...target }, success: true })),
})

test("positive controls enumerate the real endpoints, SNI variants and resolver transports", () => {
  const targets = positiveTargets(configuration, "blocked")
  expect(targets.find(value => value.label === "business-https")).toMatchObject({ address: "10.92.1.10", port: 443, expectedBody: configuration.business.expectedBody })
  expect(targets.filter(value => value.label.startsWith("metadata"))).toEqual([
    { label: "metadata-dns", transport: "dns-udp", address: configuration.resolverIPv4, port: 53, domain: configuration.denied[3].domain, expectedIPv4: "169.254.169.254" },
    { label: "metadata-numeric-tcp", transport: "tcp", address: "169.254.169.254", port: 80 },
  ])
  expect(targets.filter(value => value.label.startsWith("alternate-dns")).map(value => [value.transport, value.address, value.port])).toEqual([
    ["dns-udp", "1.1.1.1", 53], ["dns-tcp", "1.1.1.1", 53],
  ])
  expect(targets.filter(value => value.label.includes("sni")).map(value => value.label)).toContain("business-unlisted-sni")
  expect(targets.some(value => value.label.startsWith("ipv6"))).toBe(false)
  expect(positiveTargets(configuration, "ordinary").some(value => value.label.startsWith("alternate-dns"))).toBe(false)
})
test("only complete fresh successful controls for this phase and actual Host permit probing", () => {
  expect(validatePositiveReceipt(receipt(), request, now).workerHostId).toBe(request.workerHostId)
  for (const key of ["marker", "computerId", "turnId", "workerHostId"] as const) {
    const foreign = receipt(); foreign[key] = uuid("9")
    expect(() => validatePositiveReceipt(foreign, request, now)).toThrow()
  }
  const continued = receipt(); continued.phase = "continued"
  expect(() => validatePositiveReceipt(continued, request, now)).toThrow()
  const otherConfig = receipt(); otherConfig.configurationDigest = "b".repeat(64)
  expect(() => validatePositiveReceipt(otherConfig, request, now)).toThrow()
  for (const timestamp of [now - 30_001, now + 1, now - 120_001]) {
    const stale = receipt(); stale.observedAt = new Date(timestamp).toISOString()
    expect(() => validatePositiveReceipt(stale, request, now)).toThrow()
  }
})
test("a missing, duplicate, failed or changed target cannot stand in for a real positive control", () => {
  const missing = receipt(); missing.results.pop()
  expect(() => validatePositiveReceipt(missing, request, now)).toThrow()
  const duplicate = receipt(); duplicate.results[1] = duplicate.results[0]
  expect(() => validatePositiveReceipt(duplicate, request, now)).toThrow()
  const failed = receipt(); failed.results[0].success = false
  expect(() => validatePositiveReceipt(failed, request, now)).toThrow()
  const wrongPort = receipt(); wrongPort.results[0].target.port = 443
  expect(() => validatePositiveReceipt(wrongPort, request, now)).toThrow()
  const wrongAddress = receipt(); wrongAddress.results[0].target.address = "10.0.0.1"
  expect(() => validatePositiveReceipt(wrongAddress, request, now)).toThrow()
  const wrongBody = receipt(); wrongBody.results[1].target.expectedBody = "unrelated"
  expect(() => validatePositiveReceipt(wrongBody, request, now)).toThrow()
  const wrongSNI = receipt(); wrongSNI.results[2].target.servername = "other.example.org"
  expect(() => validatePositiveReceipt(wrongSNI, request, now)).toThrow()
})
test("unchanged synthetic configuration cannot start live qualification", () => {
  expect(() => requireConfigured(configuration)).toThrow("Replace synthetic fixture domains")
})

test("positive controls require fresh native placement and retain the exact lease", () => {
  expect(validatePositiveReceipt(receipt(), request, now).placementLease).toMatchObject({ epoch: 1, worker_epoch: 1, computer_instance_id: uuid("5") })
  for (const mismatch of ["computer", "host", "stale", "fenced", "lost", "expired", "newer-lease", "duplicate-epoch", "empty"] as const) {
    const wrong = receipt(), path = wrong.placement.observation
    if (mismatch === "computer") path.computer.id = uuid("9")
    if (mismatch === "host") path.leases[0].worker_host_id = uuid("9")
    if (mismatch === "stale") path.observed_at = new Date(now - 31_000).toISOString()
    if (mismatch === "fenced") path.leases[0].fenced_at = new Date(now - 1000).toISOString()
    if (mismatch === "lost") path.leases[0].status = "lost"
    if (mismatch === "expired") path.leases[0].expires_at = new Date(now - 3000).toISOString()
    if (mismatch === "newer-lease") path.leases.push({ ...path.leases[0], epoch: 2, worker_host_id: uuid("9") })
    if (mismatch === "duplicate-epoch") path.leases.push({ ...path.leases[0] })
    if (mismatch === "empty") path.leases = []
    expect(() => validatePositiveReceipt(wrong, request, now)).toThrow()
  }
  const continued = { ...request, phase: "continued" as const, workerHostId: uuid("9") }
  const replacement = receipt()
  replacement.phase = "continued"; replacement.workerHostId = continued.workerHostId
  replacement.placement.observation.leases[0].status = "lost"
  replacement.placement.observation.leases[0].fenced_at = new Date(now - 3000).toISOString()
  replacement.placement.observation.leases.push({ ...receipt().placement.observation.leases[0], epoch: 2, worker_epoch: 3,
    computer_instance_id: uuid("6"), worker_host_id: continued.workerHostId })
  expect(validatePositiveReceipt(replacement, continued, now).placementLease).toMatchObject({ epoch: 2, worker_epoch: 3, computer_instance_id: uuid("6") })
})
