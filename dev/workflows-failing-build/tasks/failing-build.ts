import { image, task, sandbox } from "@helmr/sdk"

const failing = image("helmr-intentional-build-failure")
  .from("node:24-bookworm-slim")
  .run(["sh", "-ceu", "printf 'intentional build failure\\n' >&2; exit 42"])

export const failingBuildWorkspace = sandbox({ id: "helmr-intentional-build-failure" })
  .image(failing)
  .resources({ cpu: 1, memory: "1GiB" })

export const failingBuild = task({
  id: "intentional-build-failure",
  run: () => null,
})
