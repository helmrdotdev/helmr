import { agent, computer, image, source } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

const base = image("hello-world")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")
  .run(["npm", "install", "-g", "bun@1.3.13"])
  .copy(source.file("package.json"), "/opt/helmr-task/package.json")
  .workdir("/opt/helmr-task")
  .run(["bun", "install"])
  .workdir("/sandbox")

export const helloWorldComputer = computer({
  id: "hello-world",
  image: base,
  resources: { cpu: 1, memory: "1GiB" },
})


export const helloWorld = agent({
  computer: helloWorldComputer,
  id: "hello-world",
  maxTurnDuration: "5m",
  async turn(turn) {
    const name = turn.input.map(part => part.text).join("").trim() || "Helmr"
    const greeting = `hello ${name}`
    await writeFile("hello.txt", `${greeting}\nturn=${turn.id}\n`)
    return { greeting, turnId: turn.id }
  },
})
