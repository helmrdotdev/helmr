import assert from "node:assert/strict"
import { createHash, X509Certificate } from "node:crypto"
import { Resolver } from "node:dns/promises"
import { readFile, writeFile } from "node:fs/promises"
import { request } from "node:https"
import { connect } from "node:net"
import { connect as tlsConnect, createSecureContext } from "node:tls"

const markerFile = "/workspace/private-execution-policy-preparation.json"
export const configurationDigest = config => createHash("sha256").update(JSON.stringify(config)).digest("hex")

// Any TCP or TLS connection counts as a bypass. TLS negatives deliberately ignore
// certificate identity so rejection of an unrelated certificate cannot hide access.
export async function socket(address, port, { tls = false, servername } = {}) {
  await new Promise((resolve, reject) => {
    const connection = tls ? tlsConnect({ host: address, port, servername, rejectUnauthorized: false }) : connect({ host: address, port })
    const finish = error => { clearTimeout(timer); connection.removeAllListeners(); connection.destroy(); error ? reject(error) : resolve() }
    const timer = setTimeout(() => finish(new Error("probe-timeout")), 5000)
    connection.once(tls ? "secureConnect" : "connect", () => finish())
    connection.once("error", finish)
    connection.once("close", () => finish(new Error("closed-before-connect")))
  })
}
export async function dns(domain, server, tcp = false) {
  // node:dns uses UDP with ordinary TCP fallback; explicit TCP is tested below.
  if (tcp) return socket(server, 53)
  const resolver = new Resolver({ timeout: 1000, tries: 1 })
  if (server) resolver.setServers([server])
  const timer = setTimeout(() => resolver.cancel(), 5000)
  try { return await resolver.resolve4(domain) } finally { clearTimeout(timer) }
}
export async function https(config, port = 443) {
  return await new Promise((resolve, reject) => {
    const req = request({ hostname: config.business.domain, port, path: config.business.path,
      ca: config.testCA, rejectUnauthorized: true, agent: false, headers: { Connection: "close" } }, response => {
      // Receiving headers already proves access. Validate content only after this
      // operation resolves, so a bad status/body cannot masquerade as policy denial.
      const parts = []
      response.on("data", chunk => { parts.push(chunk); if (parts.reduce((n, part) => n + part.length, 0) > 4096) response.destroy(new Error("canary-body-too-large")) })
      response.once("end", () => { clearTimeout(timer); resolve({ status: response.statusCode, body: Buffer.concat(parts).toString("utf8") }) })
      response.once("error", error => { clearTimeout(timer); reject(error) })
    })
    const timer = setTimeout(() => req.destroy(new Error("probe-timeout")), 5000)
    req.once("error", error => { clearTimeout(timer); reject(error) })
    req.end()
  })
}
export async function denied(label, operation) {
  try { await operation() } catch (error) { return { label, outcome: "denied", reason: error.code ?? error.message } }
  throw new Error(`${label}: connection bypassed the network policy`)
}
export async function probe(config, allowed, finite) {
  // Reject malformed trust even in blocked mode: an invalid CA must never become
  // false evidence that policy blocked HTTPS.
  assert(config.testCA.includes("-----BEGIN CERTIFICATE-----"), "Configure the public test CA certificate")
  assert.equal(new X509Certificate(config.testCA).ca, true, "Test trust must be a CA certificate")
  createSecureContext({ ca: config.testCA })
  const decisions = []
  if (allowed) {
    const addresses = await dns(config.business.domain)
    assert(addresses.includes(config.business.ipv4), "Business DNS did not return the configured canary")
    const result = await https(config)
    assert.equal(result.status, 200, "Business HTTPS status")
    assert.equal(result.body, config.business.expectedBody, "Business HTTPS body")
    decisions.push({ label: "business-https", outcome: "allowed", ...result, addresses })
    await socket(config.business.ipv4, 443, { tls: true, servername: config.business.domain })
    decisions.push({ label: "business-allowed-sni", outcome: "allowed" })
  } else {
    decisions.push(await denied("business-dns", () => dns(config.business.domain)))
    // Disable certificate validation in denial probes; the allowed phase separately
    // establishes certificate trust and exact HTTP status/body.
    decisions.push(await denied("business-https", () => socket(config.business.ipv4, 443, { tls: true, servername: config.business.domain })))
  }
  for (const target of config.denied) {
    if (finite || !allowed) decisions.push(await denied(`${target.kind}-dns`, () => dns(target.domain)))
    else {
      const addresses = await dns(target.domain)
      assert(addresses.includes(target.ipv4), `${target.kind}: ordinary DNS did not resolve the configured target`)
      decisions.push({ label: `${target.kind}-dns`, outcome: "resolved", addresses })
    }
    decisions.push(await denied(`${target.kind}-numeric-tcp`, () => socket(target.ipv4, target.port)))
    // Metadata has only its real HTTP/TCP 80 prerequisite.
    if (target.kind !== "metadata") decisions.push(await denied(`${target.kind}-allowed-sni`, () => socket(target.ipv4, 443, { tls: true, servername: target.domain })))
  }
  if (finite || !allowed) {
    decisions.push(await denied("business-no-sni", () => socket(config.business.ipv4, 443, { tls: true })))
    decisions.push(await denied("business-unlisted-sni", () => socket(config.business.ipv4, 443, { tls: true, servername: "unlisted.private.example" })))
    decisions.push(await denied("alternate-dns-udp", () => dns(config.alternateDNS.query, config.alternateDNS.address)))
    decisions.push(await denied("alternate-dns-tcp", () => dns(config.alternateDNS.query, config.alternateDNS.address, true)))
  }
  const ipv6Routes = await readFile("/proc/net/ipv6_route", "utf8")
  const usableDefault = ipv6Routes.split("\n").some(line => {
    const fields = line.trim().split(/\s+/), flags = Number.parseInt(fields[8] ?? "", 16)
    return fields[0] === "00000000000000000000000000000000" && fields[1] === "00" && (flags & 1) !== 0 && (flags & 0x200) === 0
  })
  assert.equal(usableDefault, false, "Guest has a usable IPv6 default route")
  decisions.push({ label: "ipv6-default-route", outcome: "absent" })
  decisions.push(await denied("ipv6-tcp", () => socket(config.ipv6, 443)))
  return { allowed, finite, configurationDigest: configurationDigest(config), decisions }
}

if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  const [raw, phase, subject] = process.argv.slice(2)
  const config = JSON.parse(raw)
  assert(["prepare", "runtime"].includes(phase), "Expected prepare or runtime")
  assert(["ordinary", "finite", "blocked"].includes(subject), "Expected ordinary, finite or blocked")
  const allowed = phase === "prepare" ? subject === "blocked" : subject !== "blocked"
  const report = await probe(config, allowed, phase === "prepare" || subject === "finite")
  if (phase === "prepare") await writeFile(markerFile, JSON.stringify({ phase, subject, ...report }), { flag: "wx", mode: 0o600 })
  else if (subject !== "ordinary") {
    report.preparation = JSON.parse(await readFile(markerFile, "utf8"))
    assert.equal(report.preparation.subject, subject)
    assert.equal(report.preparation.allowed, !allowed)
    assert.equal(report.preparation.configurationDigest, report.configurationDigest)
  }
  console.log(JSON.stringify({ phase, subject, ...report }))
}
