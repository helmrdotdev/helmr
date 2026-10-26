import { spawn } from "node:child_process"

// This is a test-owned process boundary, not a policy for the native adapter.
// PID 1 exits with the fixture or when its private control pipe closes. Waiting
// for PID 1 joins kernel teardown of every descendant, even new sessions.
export async function runInLinuxProcessScope(command: string[], env: NodeJS.ProcessEnv, signal: AbortSignal): Promise<void> {
  signal.throwIfAborted()
  const supervisor = `
    const { spawn } = require("node:child_process");
    const command = ${JSON.stringify(command)};
    const child = spawn(command[0], command.slice(1), { stdio: ["ignore", "inherit", "inherit"] });
    child.once("error", error => { console.error(error); process.exit(1); });
    child.once("exit", (code, signal) => process.exit(code ?? 1));
    process.stdin.once("end", () => process.exit(125));
    process.stdin.resume();
  `
  const child = spawn("bwrap", [
    "--unshare-user", "--unshare-pid", "--as-pid-1", "--die-with-parent",
    "--bind", "/", "/", "--dev-bind", "/dev", "/dev", "--proc", "/proc", "--", process.execPath, "-e", supervisor,
  ], { env, stdio: ["pipe", "inherit", "inherit"] })
  // Keep the outer monitor alive: its wait is the namespace teardown barrier.
  const abort = () => { child.stdin.end() }
  signal.addEventListener("abort", abort, { once: true })
  let error: Error | undefined
  child.once("error", reason => { error = reason })
  try {
    const result = await new Promise<{ code: number | null; signal: NodeJS.Signals | null }>(resolve => {
      child.once("close", (code, signal) => resolve({ code, signal }))
    })
    signal.throwIfAborted()
    if (error) throw error
    if (result.code !== 0) throw new Error(`Linux fixture exited: code=${result.code} signal=${result.signal}`)
  } finally {
    signal.removeEventListener("abort", abort)
  }
}
