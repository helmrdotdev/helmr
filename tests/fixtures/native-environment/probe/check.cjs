// Run by the guestd launch test inside each Workspace root, with the admitted
// Program and Runtime mounted exactly as a managed Program sees them.
const { execFileSync, spawnSync } = require("node:child_process")
const dns = require("node:dns")
const fs = require("node:fs")
const path = require("node:path")

const program = path.resolve(__dirname, "..")
const load = (file, use) => {
  try {
    const addon = require(path.join(program, "build/Release", file))
    return { ok: true, value: use ? JSON.parse(addon.run()) : addon.value }
  } catch (error) {
    return { ok: false, error: String(error.message).split("\n")[0] }
  }
}
const family = /\/(ld-linux-x86-64|libc|libm|libmvec|libdl|libpthread|librt|libutil|libanl|libresolv|libnss_[a-z]+|libgcc_s|libstdc\+\+)[.-][^/]*$/
const foreignFamily = () => [...new Set(fs.readFileSync("/proc/self/maps", "utf8").split("\n")
  .map(line => line.split(/\s+/)[5])
  .filter(file => file && family.test(file) && !file.startsWith("/opt/helmr/runtime/lib/")))]

const result = {
  execPath: process.execPath,
  uid: process.getuid(),
  lifecycle: JSON.parse(fs.readFileSync(path.join(program, "generated/lifecycle.json"), "utf8")),
  distro: load("distro_addon.node", true),
  vendored: load("vendored/addon.node", true),
  future: load("future_addon.node", false),
  child: execFileSync(process.execPath, ["-p", "JSON.stringify(Object.keys(process.env).filter(k => k.startsWith('LD_')))"], { encoding: "utf8" }).trim(),
}
const opencode = spawnSync(path.join(program, "node_modules/.bin/opencode"), ["--version"], { encoding: "utf8" })
result.opencode = { status: opencode.status, stdout: (opencode.stdout || "").trim(), error: opencode.error ? opencode.error.code : "" }
dns.lookup("localhost", error => {
  result.dns = error ? error.code : "ok"
  result.foreignFamily = foreignFamily()
  process.stdout.write(JSON.stringify(result) + "\n")
})
