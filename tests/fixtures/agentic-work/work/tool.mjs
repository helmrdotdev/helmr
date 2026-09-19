// A deterministic stand-in for an agent's command-line tool.
import { spawn } from "node:child_process"

const [mode, marker] = process.argv.slice(2)
if (mode === "sum") {
  let input = ""
  for await (const chunk of process.stdin) input += chunk
  const { values } = JSON.parse(input)
  process.stdout.write(JSON.stringify({ count: values.length, sum: values.reduce((sum, value) => sum + value, 0) }))
} else if (mode === "fail") {
  console.error("tool: refusing malformed request")
  process.exitCode = 3
} else if (mode === "hang") {
  spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)", marker], { stdio: "ignore" })
  process.stdout.write("started\n")
  setInterval(() => {}, 1000)
}
