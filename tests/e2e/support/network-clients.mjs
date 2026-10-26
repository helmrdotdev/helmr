// Runs unchanged in a root-owned local namespace or a Helmr Computer.
// All destinations and credentials below belong to the synthetic TLS fixture.
import { createHash, randomUUID } from "node:crypto"
import { spawn } from "node:child_process"
import { lookup } from "node:dns/promises"
import { readFile, writeFile, mkdtemp, rm } from "node:fs/promises"
import https from "node:https"
import { tmpdir } from "node:os"
import path from "node:path"
import tls from "node:tls"
import { createRequire } from "node:module"
import { pathToFileURL } from "node:url"

const configFile = process.env.HELMR_NETWORK_CLIENT_CONFIG
if (!configFile || !process.env.HELMR_NETWORK_CLIENT_SDK_ROOT) {
  throw new Error("Fixture prerequisite: HELMR_NETWORK_CLIENT_CONFIG and HELMR_NETWORK_CLIENT_SDK_ROOT must identify prepared local TLS fixture artifacts; no public endpoint defaults are used")
}
const config = JSON.parse(await readFile(configFile, "utf8"))
const caFile = path.resolve(path.dirname(configFile), config.caFile)
const ca = await readFile(caFile)
if (!/^[a-f0-9]{64}$/.test(config.fixtureBinarySHA256 ?? "")) throw new Error("Fixture prerequisite: missing endpoint executable identity")
if (process.env.NODE_EXTRA_CA_CERTS !== caFile) {
  throw new Error("Fixture prerequisite: start Node with NODE_EXTRA_CA_CERTS set to the fixture CA file; the script never installs a system CA")
}
for (const name of ["model", "git", "registry", "auth", "cdn", "private"]) {
  const origin = new URL(config.origins[name])
  if (origin.hostname !== `${name}.network.test` || origin.protocol !== "https:" || origin.port && origin.port !== "443" || origin.username || origin.password || origin.pathname !== "/" || origin.search || origin.hash) {
    throw new Error(`Invalid fixture HTTPS/443 origin: ${name}`)
  }
}
const work = await mkdtemp(path.join(tmpdir(), "helmr-network-clients-"))
const syntheticToken = "helmr-network-client-synthetic-token"
const childEnv = {
  PATH: process.env.PATH, HOME: work, TMPDIR: work,
  GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: path.join(work, "gitconfig"),
  GIT_TERMINAL_PROMPT: "0", GIT_SSL_CAINFO: caFile,
  NPM_CONFIG_USERCONFIG: path.join(work, "npmrc"), NPM_CONFIG_GLOBALCONFIG: path.join(work, "npm-globalrc"),
  NPM_CONFIG_CACHE: path.join(work, "npm-cache"), NPM_CONFIG_UPDATE_NOTIFIER: "false",
  NODE_EXTRA_CA_CERTS: caFile,
}
async function command(executable, args, expectSuccess = true) {
  return await new Promise((resolve, reject) => {
    const child = spawn(executable, args, { env: childEnv, cwd: work, stdio: ["ignore", "pipe", "pipe"], timeout: 30_000 })
    let output = "", overflow = false
    const collect = data => { if (output.length + data.length > 64 * 1024) { overflow = true; child.kill() } else output += data.toString() }
    child.stdout.on("data", collect); child.stderr.on("data", collect)
    child.on("error", reject)
    child.on("close", code => {
      if (overflow || code === null || expectSuccess !== (code === 0)) reject(new Error(`${executable} fixture check failed (exit ${code}, bounded output ${overflow ? "exceeded" : "captured"})`))
      else resolve(output.trim())
    })
  })
}
async function request(url, method = "GET", trust = ca) {
  return await new Promise((resolve, reject) => {
    const req = https.request(url, { method, ca: trust, timeout: 10_000 }, response => {
      let body = ""
      response.on("data", data => { body += data; if (body.length > 64 * 1024) req.destroy(new Error("fixture response exceeds bound")) })
      response.on("end", () => resolve({ status: response.statusCode, body, location: response.headers.location }))
    })
    req.on("error", reject); req.on("timeout", () => req.destroy(new Error("fixture request deadline"))); req.end()
  })
}
try {
  await writeFile(childEnv.GIT_CONFIG_GLOBAL, "")
  await writeFile(childEnv.NPM_CONFIG_GLOBALCONFIG, "")
  const registry = new URL(config.origins.registry)
  await writeFile(childEnv.NPM_CONFIG_USERCONFIG, `registry=${registry.href}\n//${registry.host}/:_authToken=${syntheticToken}\ncafile=${caFile}\nstrict-ssl=true\naudit=false\nfund=false\nupdate-notifier=false\n`, { mode: 0o600 })
  const sdkRoot = path.resolve(process.env.HELMR_NETWORK_CLIENT_SDK_ROOT)
  const sdkPackageBytes = await readFile(path.join(sdkRoot, "package.json"))
  const sdkPackage = JSON.parse(sdkPackageBytes)
  const versions = { node: process.version, npm: await command("npm", ["--version"]), git: await command("git", ["--version"]), anthropic: sdkPackage.version }
  for (const [name, expected] of Object.entries(config.expectedVersions)) {
    if (versions[name] !== expected) throw new Error(`Client version mismatch for ${name}: expected ${expected}, got ${versions[name]}`)
  }
  const { default: Anthropic } = await import(pathToFileURL(path.join(sdkRoot, "index.mjs")).href)
  const model = new Anthropic({ apiKey: syntheticToken, authToken: null, webhookKey: null, baseURL: config.origins.model, maxRetries: 0, timeout: 15_000, logLevel: "off" })
  const streamProbe = randomUUID()
  const stream = await model.messages.create({ model: "synthetic-network-fixture", max_tokens: 8, messages: [{ role: "user", content: "synthetic fixture" }], stream: true }, { headers: { "x-helmr-stream-probe": streamProbe } })
  let text = "", acknowledged = false
  for await (const event of stream) {
    if (event.type !== "content_block_delta" || event.delta.type !== "text_delta") continue
    text += event.delta.text
    if (text === "first" && !acknowledged) {
      const ack = await request(config.origins.model + "/stream-ack/" + streamProbe, "POST")
      if (ack.status !== 204) throw new Error("Native SDK did not acknowledge the first streamed chunk")
      acknowledged = true
    }
  }
  if (!acknowledged || text !== "firstsecond") throw new Error("Native model SDK stream was incomplete or buffered until EOF")
  const clone = path.join(work, "clone")
  await command("git", ["-c", "credential.helper=", "clone", "--quiet", "--no-checkout", config.origins.git + "/repo.git", clone])
  if (await command("git", ["-C", clone, "rev-parse", "HEAD"]) !== config.gitHead) throw new Error("Git HTTPS clone did not retain the fixture commit")
  const install = path.join(work, "package")
  const npmArgs = ["install", "--prefix", install, "--ignore-scripts", "--package-lock=false", "--no-audit", "--no-fund", "--fetch-retries=0", "--fetch-timeout=5000"]
  await command("npm", [...npmArgs, "@helmr/network-client-fixture@1.0.0"])
  const require = createRequire(path.join(install, "check.cjs"))
  if (require("@helmr/network-client-fixture").fixture !== true) throw new Error("npm registry/auth/CDN chain returned wrong package bytes")
  const deniedOutput = await command("npm", [...npmArgs, "--prefix", path.join(work, "denied-package"), "--cache", path.join(work, "denied-cache"), "@helmr/network-client-denied@1.0.0"], false)
  if (!/(ENOTFOUND|EAI_AGAIN|ECONNREFUSED|EHOSTUNREACH|ETIMEDOUT)/.test(deniedOutput) || !deniedOutput.includes("denied.network.test")) throw new Error("npm denial did not fail at the forbidden redirect destination")
  const { address: endpointIPv4 } = await lookup(registry.hostname, { family: 4 })
  await new Promise((resolve, reject) => {
    const socket = tls.connect({ host: endpointIPv4, port: 443, servername: "denied.network.test", ca, timeout: 10_000 })
    socket.on("secureConnect", () => { socket.destroy(); reject(new Error("Numeric-IP TLS bypassed the forbidden SNI policy")) })
    socket.on("error", error => {
      if (["ECONNRESET", "ECONNREFUSED", "EPIPE"].includes(error.code)) resolve()
      else reject(error)
    })
    socket.on("timeout", () => socket.destroy(new Error("Numeric-IP TLS denial deadline")))
  })
  let untrustedRejected = false
  try { await request(config.origins.private + "/private", "GET", []) } catch { untrustedRejected = true }
  if (!untrustedRejected || (await request(config.origins.private + "/private")).body !== "private fixture") throw new Error("Private HTTPS must require explicit fixture CA trust")
  const reportResponse = await request(config.origins.registry + "/__report")
  if (reportResponse.status !== 200) throw new Error("Fixture report unavailable")
  const report = JSON.parse(reportResponse.body)
  for (const check of ["streamAcknowledged", "gitRead", "registryAuthenticated", "authRedirect", "cdnRead", "privateRead", "deniedRedirect"]) {
    if (!report[check]) throw new Error(`Fixture endpoint did not observe ${check}`)
  }
  if (report.crossOriginCredentials !== 0 || report.deniedOriginContacts !== 0) throw new Error("Fixture observed credential forwarding or a forbidden redirect contact")
  console.log(JSON.stringify({ syntheticFixture: true, liveModelProvider: false, versions, fixtureBinarySHA256: config.fixtureBinarySHA256, sdkPackageSHA256: createHash("sha256").update(sdkPackageBytes).digest("hex"), caSHA256: createHash("sha256").update(ca).digest("hex"), runnerSHA256: createHash("sha256").update(await readFile(new URL(import.meta.url))).digest("hex"), checks: { nativeModelStreaming: true, gitHTTPS: true, npmAuthenticatedRedirectChain: true, deniedNpmRedirect: true, deniedNumericTLS: true, privateExplicitCA: true }, report }))
} finally {
  await rm(work, { recursive: true, force: true })
}
