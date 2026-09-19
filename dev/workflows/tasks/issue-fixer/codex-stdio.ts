import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process"
import { createRequire } from "node:module"
import { createInterface } from "node:readline"
import { Queue } from "./queue"

export type NativeMessage = { id?: string | number; method: string; params?: unknown }

// Small app-server wire transport. Provider policy stays in codex.ts.
export class CodexStdio {
  readonly messages = new Queue<NativeMessage>()
  private readonly pending = new Map<number, { resolve: (value: unknown) => void; reject: (error: Error) => void }>()
  private readonly exited: Promise<void>
  private nextID = 1
  private closed = false

  constructor(
    observe: (message: NativeMessage) => void = () => {},
    private readonly child: ChildProcessWithoutNullStreams = spawn(process.execPath,
      [createRequire(import.meta.url).resolve("@openai/codex/bin/codex.js"), "app-server", "--listen", "stdio://"]),
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
          if (message.error) waiter.reject(new Error(JSON.stringify(message.error)))
          else waiter.resolve(message.result)
        }
      } catch (error) { fail(error instanceof Error ? error : new Error(String(error))) }
    })
  }

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
