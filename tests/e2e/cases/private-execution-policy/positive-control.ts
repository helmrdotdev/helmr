import assert from "node:assert/strict"
import { X509Certificate } from "node:crypto"
import { z } from "zod"
import type { configuration } from "./config"

type Configuration = typeof configuration
export type Subject = "ordinary" | "finite" | "blocked"
const target = z.object({
  label: z.string(), transport: z.enum(["tcp", "tls", "https", "dns-udp", "dns-tcp"]),
  address: z.ipv4(), port: z.number().int().min(1).max(65535),
  domain: z.string().optional(), servername: z.string().optional(),
  expectedIPv4: z.ipv4().optional(), path: z.string().optional(), expectedBody: z.string().optional(),
}).strict()
export type PositiveTarget = z.infer<typeof target>
export function requireConfigured(config: Configuration) {
  for (const domain of [config.business.domain, ...config.denied.map(value => value.domain)]) {
    assert(!/(?:^|\.)(?:example|invalid|test)$/.test(domain), "Replace synthetic fixture domains before qualification")
  }
  assert.equal(new X509Certificate(config.testCA).ca, true, "Configure a valid public test CA certificate")
}
export function positiveTargets(config: Configuration, subject: Subject): PositiveTarget[] {
  const business = config.business
  const result: PositiveTarget[] = [
    { label: "business-dns", transport: "dns-udp", address: config.resolverIPv4, port: 53, domain: business.domain, expectedIPv4: business.ipv4 },
    { label: "business-https", transport: "https", address: business.ipv4, port: 443, domain: business.domain, path: business.path, expectedBody: business.expectedBody },
    { label: "business-allowed-sni", transport: "tls", address: business.ipv4, port: 443, servername: business.domain },
  ]
  for (const denied of config.denied) {
    result.push({ label: `${denied.kind}-dns`, transport: "dns-udp", address: config.resolverIPv4, port: 53, domain: denied.domain, expectedIPv4: denied.ipv4 })
    result.push({ label: `${denied.kind}-numeric-tcp`, transport: "tcp", address: denied.ipv4, port: denied.port })
    if (denied.kind !== "metadata") result.push({ label: `${denied.kind}-allowed-sni`, transport: "tls", address: denied.ipv4, port: 443, servername: denied.domain })
  }
  if (subject !== "ordinary") result.push(
    { label: "business-no-sni", transport: "tls", address: business.ipv4, port: 443 },
    { label: "business-unlisted-sni", transport: "tls", address: business.ipv4, port: 443, servername: "unlisted.private.example" },
    { label: "alternate-dns-udp", transport: "dns-udp", address: config.alternateDNS.address, port: 53, domain: config.alternateDNS.query },
    { label: "alternate-dns-tcp", transport: "dns-tcp", address: config.alternateDNS.address, port: 53, domain: config.alternateDNS.query },
  )
  return result.map(value => target.parse(value))
}
const request = z.object({
  marker: z.uuid(), phase: z.enum(["first", "continued"]), configurationDigest: z.string().regex(/^[a-f0-9]{64}$/),
  computerId: z.uuid(), turnId: z.uuid(), workerHostId: z.uuid(), requestedAt: z.iso.datetime(),
  targets: z.array(target).min(1), ipv6: z.literal("guest-disabled-topology"),
}).strict()
export type PositiveRequest = z.infer<typeof request>
const leaseObservation = z.object({
  observed_at: z.iso.datetime({ offset: true }),
  computer: z.object({ id: z.uuid() }).passthrough(),
  leases: z.array(z.object({
    epoch: z.number().int().positive(), worker_epoch: z.number().int().positive(),
    computer_instance_id: z.uuid(), worker_host_id: z.uuid(), status: z.string(),
    fenced_at: z.iso.datetime({ offset: true }).nullable(), expires_at: z.iso.datetime({ offset: true }),
  }).passthrough()).min(1),
}).passthrough()
export function validatePositiveReceipt(value: unknown, expected: PositiveRequest, now = Date.now()) {
  request.parse(expected)
  const receipt = request.extend({ observedAt: z.iso.datetime(), evidencePath: z.string().min(1),
    placement: z.object({ evidencePath: z.string().min(1), observation: leaseObservation }).strict(),
    results: z.array(z.object({ target, success: z.literal(true) }).strict()).min(1),
  }).strict().parse(value)
  const { observedAt, evidencePath, results, placement, ...binding } = receipt
  assert.deepEqual(binding, expected, "Positive controls belong to another attempt, phase, configuration, Worker Host or target set")
  const observed = Date.parse(observedAt)
  assert(observed >= Date.parse(expected.requestedAt) && observed <= now && now - observed <= 120_000, "Positive controls must be observed after this request and within two minutes")
  const path = placement.observation
  assert.equal(path.computer.id, expected.computerId, "Placement observes another Computer")
  const at = Date.parse(path.observed_at)
  assert(at >= Date.parse(expected.requestedAt) && at <= now && now - at <= 120_000, "Placement must be observed for this phase within two minutes")
  assert.equal(new Set(path.leases.map(lease => lease.epoch)).size, path.leases.length, "Placement lease epochs are ambiguous")
  const lease = path.leases.reduce((latest, candidate) => candidate.epoch > latest.epoch ? candidate : latest)
  assert(lease.status === "active" && lease.fenced_at === null && Date.parse(lease.expires_at) > at, "Latest placement is not an active unfenced lease")
  assert.equal(lease.worker_host_id, expected.workerHostId, "Latest placement uses another Worker Host")
  const canonical = (targets: PositiveTarget[]) => [...targets].sort((a, b) => a.label.localeCompare(b.label))
  assert.deepEqual(canonical(results.map(result => result.target)), canonical(expected.targets), "Every exact target/transport/port requires a successful positive control")
  return { ...receipt, placementLease: lease }
}
