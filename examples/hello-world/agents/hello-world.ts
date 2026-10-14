import { agent, template, image } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

const base = image("hello-world")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")

export const helloWorldComputer = template({
  id: "hello-world",
  image: base,
  resources: { cpu: 1, memory: "1GiB" },
})


export const helloWorld = agent({
  template: helloWorldComputer,
  id: "hello-world",
  maxTurnDuration: "5m",
  async turn(turn) {
    const name = turn.input.map(part => part.text).join("").trim() || "Helmr"
    const greeting = `hello ${name}`
    await writeFile("hello.txt", `${greeting}\nturn=${turn.id}\n`)
    return { greeting, turnId: turn.id }
  },
})
