// Ordinary project lifecycle work, run by the package manager as the
// unprivileged install user after node-gyp compiled the root addon.
import { execFileSync } from "node:child_process"
import { copyFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { createRequire } from "node:module"
import assert from "node:assert/strict"

const require = createRequire(import.meta.url)
const run = (command, args) => execFileSync(command, args, { stdio: ["ignore", "pipe", "inherit"], encoding: "utf8" })

assert.equal(process.getuid(), 65532, "dependency installation must not run as root")
// Output derived from a project source file, so the install cache must follow
// it. jq exists only because build.builder installed it before this install ran.
mkdirSync("generated", { recursive: true })
writeFileSync("generated/lifecycle.json",
  run("jq", ["--compact-output", "--raw-input", "{input: .}", "lifecycle-input.txt"]).trim())
assert.equal(readFileSync("/etc/helmr-recipe-note", "utf8").startsWith("prepared with: recipe input"), true)
copyFileSync("/etc/helmr-environment-stamp", "generated/environment-stamp.txt")

// node-gyp compiled the root addon against the builder image's SQLite headers.
const distro = JSON.parse(require("../build/Release/distro_addon.node").run())
assert.equal(distro.resolver, 0)

// A dependency's own postinstall already ran; its executable works here.
assert.equal(run("node_modules/.bin/opencode", ["--version"]).trim(), "1.18.30")

const include = `${process.execPath.replace(/\/bin\/node$/, "")}/include/node`
const release = "build/Release"

// Control: the same addon carrying its library beside itself.
mkdirSync(`${release}/vendored/lib`, { recursive: true })
run("gcc", ["-shared", "-fPIC", `-I${include}`, "native/addon.c", "-Wl,--no-as-needed", "-lrt", "-lresolv", "-lsqlite3",
  "-Wl,-rpath,$ORIGIN/lib", "-o", `${release}/vendored/addon.node`])
copyFileSync("/usr/lib/x86_64-linux-gnu/libsqlite3.so.0", `${release}/vendored/lib/libsqlite3.so.0`)

// Negative: an object that needs a glibc symbol version newer than any Runtime.
mkdirSync(`${release}/future-libc`, { recursive: true })
run("gcc", ["-shared", "-fPIC", "native/future-libc.c", "-Wl,-soname,libc.so.6",
  "-Wl,--version-script=native/future-libc.map", "-o", `${release}/future-libc/libc.so.6`])
run("gcc", ["-shared", "-fPIC", `-I${include}`, "native/future.c", `-L${release}/future-libc`, "-l:libc.so.6",
  "-o", `${release}/future_addon.node`])

// The e2e project has no registry SDK; provide the declaration surface it uses.
mkdirSync("node_modules/@helmr/sdk", { recursive: true })
writeFileSync("node_modules/@helmr/sdk/package.json", JSON.stringify({ name: "@helmr/sdk", type: "module" }))
writeFileSync("node_modules/@helmr/sdk/index.js", `
const brand = Symbol.for("helmr.sdk.v0.definition")
export function defineConfig(config) { return config }
export function task(config) {
  return Object.freeze({ [brand]: Object.freeze({ kind: "task", id: config.id, hasPayload: false, handler: config.run }) })
}
`)
