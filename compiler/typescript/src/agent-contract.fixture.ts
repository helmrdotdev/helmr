import { agent, template, image, triggers } from "@helmr/sdk"
import { analyze } from "./compile"

export function agentContractFixture() {
  const workspace = template({
    id: "workspace", image: image("base").from("ubuntu:24.04"),
    resources: { cpu: 2, memory: "4GiB", disk: "16GiB" },
    prepare: async build => { await build.exec(["true"]) },
    refresh: { every: "24h", maxAge: "48h" },
    secrets: [{ secret: "API_TOKEN", env: { name: "API_TOKEN", mode: "raw" } }],
    prepareSecrets: [{ secret: "SOURCE_TOKEN", env: { name: "SOURCE_TOKEN", mode: "raw" } }],
  })
  const coder = agent({
    id: "coder", template: workspace, init: () => null, turn: () => null,
    maxTurnDuration: "5m", closeAfterIdle: "1h", hibernateAfterIdle: "10m",
    triggers: [triggers.cron("nightly", "0 0 * * *", { timezone: "Asia/Tokyo", input: [{ type: "text", text: "Run the nightly check." }] })],
  })
  return analyze({ architecture: "x86_64", exports: [{ modulePath: "helmr/app/entry-0.mjs", exportName: "coder", value: coder }] })
}
