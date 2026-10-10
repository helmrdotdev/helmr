import { spawnNative as spawnManagedNative, type NativeResource, type NativeProcess } from "@helmr/sdk/internal"
import type { ChildProcessWithoutNullStreams } from "node:child_process"
import { dirname, join, delimiter } from "node:path"
import { createRequire } from "node:module"
import { createInterface } from "node:readline"
import { Queue } from "./queue"

export type NativeMessage = { id?: string | number; method: string; params?: unknown }

// Small app-server wire transport. Provider policy stays in codex-harness.ts.
export function spawnManagedCodex(resource: NativeResource, home: string, environment: NodeJS.ProcessEnv = process.env): NativeProcess {
  const options = codexCommand({ ...environment, CODEX_HOME: home })
  return spawnManagedNative(resource, options.command, options.args, { env: options.env })
}
function codexCommand(environment: NodeJS.ProcessEnv) {
  const architecture = process.arch === "arm64" ? "aarch64" : process.arch === "x64" ? "x86_64" : undefined
  const platform = process.platform === "darwin" ? "apple-darwin" : process.platform === "linux" ? "unknown-linux-musl" : undefined
  if (!architecture || !platform) throw new Error("Unsupported native test platform")
  const require = createRequire(import.meta.url)
  const packageRoot = dirname(require.resolve("@openai/codex/package.json"))
  const nativeRequire = createRequire(join(packageRoot, "package.json"))
  const root = dirname(nativeRequire.resolve(`@openai/codex-${process.platform}-${process.arch}/package.json`))
  const vendor = join(root, "vendor", `${architecture}-${platform}`)
  return { command: join(vendor, "bin", "codex"), args: ["app-server", "--listen", "stdio://"],
    env: { ...environment, PATH: [join(vendor, "codex-path"), environment.PATH].filter(Boolean).join(delimiter), CODEX_MANAGED_PACKAGE_ROOT: packageRoot },
  }
}
export class CodexRequestRejected extends Error {
  readonly code: number
  constructor(code: number, message: string) { super(message); this.code = code }
}

export class CodexStdio {
  readonly messages = new Queue<NativeMessage>()
  private readonly pending = new Map<number, { resolve: (value: unknown) => void; reject: (error: Error) => void }>()
  private readonly exited: Promise<void>
  private nextID = 1
  private closed = false

  constructor(
    observe: (message: NativeMessage) => void,
    private readonly child: ChildProcessWithoutNullStreams,
  ) {
    // Drain native diagnostics without mixing them into the JSON-RPC stream.
    this.child.stderr.on("data", chunk => process.stderr.write(chunk))
    const lines = createInterface({ input: this.child.stdout })
    const fail = (error: Error) => {
      this.closed = true
      for (const waiter of this.pending.values()) waiter.reject(error)
      this.pending.clear()
      this.messages.end(error)
    }
    this.exited = new Promise(resolve => {
      this.child.once("close", () => { lines.close(); fail(new Error("Codex app-server closed")); resolve() })
    })
    this.child.once("error", fail)
    this.child.stdin.on("error", fail)
    lines.on("line", line => {
      try {
        const message = JSON.parse(line)
        // Server requests have BOTH method and id. Check method first.
        if (typeof message.method === "string") {
          // Cancellation must not wait behind a slow retained-output projection.
          observe(message)
          this.messages.push(message)
        }
        else {
          const waiter = this.pending.get(message.id)
          if (!waiter) return
          this.pending.delete(message.id)
          if (message.error) waiter.reject(new CodexRequestRejected(message.error.code, message.error.message ?? "Native request rejected"))
          else waiter.resolve(message.result)
        }
      } catch (error) { fail(error instanceof Error ? error : new Error(String(error))) }
    })
  }

  get processId(): number | undefined { return this.child.pid }

  request(method: string, params: unknown): Promise<unknown> {
    if (this.closed) return Promise.reject(new Error("Codex is closed"))
    const id = this.nextID++
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      this.send({ id, method, params }).catch(error => { this.pending.delete(id); reject(error) })
    })
  }
  send(value: unknown): Promise<void> {
    if (this.closed) return Promise.reject(new Error("Codex is closed"))
    return new Promise((resolve, reject) => {
      this.child.stdin.write(`${JSON.stringify(value)}\n`, error => error ? reject(error) : resolve())
    })
  }
  async close(): Promise<void> {
    this.child.stdin.end()
    this.child.kill("SIGTERM")
    const kill = setTimeout(() => this.child.kill("SIGKILL"), 2_000)
    try { await this.exited } finally { clearTimeout(kill) }
  }
}
