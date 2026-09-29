import { spawn } from "node:child_process"
import { cp, lstat, readFile, realpath, rm, writeFile } from "node:fs/promises"
import { resolve } from "node:path"
import { contained } from "./bundle"

// npm's lock is a resolution record, not an attestation of script-created files.
// Artifact admission also verifies the actual tree and contained link targets.
export async function assembleRuntime(options: { bundle: string; installed: string; output: string }) {
  const bundle = await realpath(options.bundle)
  const installed = await realpath(options.installed)
  const lock = JSON.parse(await readFile(resolve(installed, "package-lock.json"), "utf8"))
  if (lock.lockfileVersion !== 3 || lock.packages === null || typeof lock.packages !== "object" || Array.isArray(lock.packages)) throw new Error("runtime installation requires an npm v3 lock")
  for (const [path, raw] of Object.entries(lock.packages)) {
    if (path === "") continue
    if (raw === null || typeof raw !== "object" || Array.isArray(raw)) throw new Error(`invalid runtime package record: ${path}`)
    const entry = raw as Record<string, unknown>
    if (entry["link"] || (!entry["inBundle"] && (typeof entry["resolved"] !== "string" || !entry["resolved"].startsWith("https://registry.npmjs.org/") || typeof entry["integrity"] !== "string"))) {
      throw new Error(`runtime package ${path} has an unsupported source; only public registry packages are supported`)
    }
  }
  const manifest = JSON.parse(await readFile(resolve(bundle, "install/package.json"), "utf8"))
  for (const [name, version] of Object.entries(manifest.dependencies)) {
    const path = await realpath(resolve(installed, "node_modules", name, "package.json"))
    if (!contained(installed, path)) throw new Error(`runtime external escapes installation: ${name}`)
    const actual = JSON.parse(await readFile(path, "utf8"))
    if (actual.name !== name || actual.version !== version) throw new Error(`runtime external does not match selected version: ${name}`)
  }
  await cp(resolve(bundle, "payload"), options.output, { recursive: true, errorOnExist: true, force: false })
  const modules = resolve(installed, "node_modules")
  const info = await lstat(modules).catch(error => {
    if (error.code === "ENOENT" && Object.keys(manifest.dependencies).length === 0) return undefined
    throw error
  })
  if (info !== undefined) {
    if (!info.isDirectory()) throw new Error("runtime node_modules must be a directory")
    await cp(modules, resolve(options.output, "node_modules"), { recursive: true, verbatimSymlinks: true })
    await rm(resolve(options.output, "node_modules/.package-lock.json"), { force: true })
  }
}

// Recipe environment variables must not silently change npm's install contract.
export async function installRuntimePackages() {
  const env: NodeJS.ProcessEnv = {
    HOME: "/computer/home", TMPDIR: "/computer/tmp", LANG: "C.UTF-8",
    PATH: "/opt/helmr/runtime/bin:/usr/local/bin:/usr/bin:/bin",
  }
  for (const name of ["SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "CC", "CXX", "CFLAGS", "CXXFLAGS", "LDFLAGS", "PKG_CONFIG_PATH"]) {
    if (process.env[name] !== undefined) env[name] = process.env[name]
  }
  const userConfig = "/computer/home/runtime-user.npmrc"
  const globalConfig = "/computer/home/runtime-global.npmrc"
  await writeFile(userConfig, "")
  await writeFile(globalConfig, "")
  await new Promise<void>((resolve, reject) => {
    const child = spawn(process.execPath, ["/opt/helmr/npm/bin/npm-cli.js", "install", "--no-audit", "--no-fund", "--omit=dev", "--package-lock=true", "--prefer-online", "--registry=https://registry.npmjs.org/", `--userconfig=${userConfig}`, `--globalconfig=${globalConfig}`, "--cache=/computer/npm-cache"], { env, stdio: "inherit" })
    child.once("error", reject)
    child.once("exit", code => code === 0 ? resolve() : reject(new Error(`runtime package installation failed (${code})`)))
  })
}
