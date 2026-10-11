import { agent, computer, image, triggers } from "@helmr/sdk"
import { analyze } from "./compile"

export function agentContractFixture() {
  const workspace = computer({
    id: "workspace", image: image("base").from("ubuntu:24.04"),
    resources: { cpu: 2, memory: "4GiB", disk: "16GiB" },
    prepare: async build => { await build.exec(["true"]) },
    refresh: { every: "24h", maxAge: "48h" },
    secrets: [{ secretId: "01900000-0000-7000-8000-000000000001", env: { name: "API_TOKEN", mode: "raw" } }],
    buildSecrets: [{ secretId: "01900000-0000-7000-8000-000000000002", env: { name: "SOURCE_TOKEN", mode: "raw" } }],
  })
  const coder = agent({
    id: "coder", computer: workspace, setup: () => null, turn: () => null,
    maxTurnDuration: "5m", closeAfterIdle: "1h",
    triggers: [triggers.cron("nightly", "0 0 * * *", { timezone: "Asia/Tokyo", input: [{ type: "text", text: "Run the nightly check." }] })],
  })
  return analyze({ architecture: "x86_64", exports: [{ modulePath: "helmr/app/entry-0.mjs", exportName: "coder", value: coder }] })
}
