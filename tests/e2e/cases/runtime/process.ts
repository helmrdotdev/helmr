import { execFile } from "node:child_process"
import { promisify } from "node:util"
export async function checkCommand(command: readonly string[]) {
  const env = Object.fromEntries(
    ["PATH", "HOME", "TMPDIR", "LANG"].flatMap((name) =>
      process.env[name] ? [[name, process.env[name]!]] : [],
    ),
  )
  const { stdout, stderr } = await promisify(execFile)(command[0]!, [...command.slice(1)], {
    env,
    timeout: 30_000,
    maxBuffer: 1_000_000,
  })
  return { command: command.join(" "), output: (stdout || stderr).trim().slice(0, 2000) }
}
