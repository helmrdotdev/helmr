import { cache, image, sandbox, source, task } from "@helmr/sdk"

const installNode24 = [
  "apt-get update",
  "apt-get install -y --no-install-recommends ca-certificates curl gnupg",
  "install -d -m 0755 /etc/apt/keyrings",
  "curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg",
  "echo 'deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_24.x nodistro main' > /etc/apt/sources.list.d/nodesource.list",
  "apt-get update",
  "apt-get install -y --no-install-recommends nodejs",
  "rm -rf /var/lib/apt/lists/*",
].join(" && ")

const base = image("secret-vault")
  .from("oven/bun:1.3.10-debian")
  .workdir("/workspace")
  .run(["sh", "-ceu", installNode24])
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("secret-vault-bun") }],
  })

const sbx = sandbox("secret-vault")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

export const useSecret = task({
  id: "use-secret",
  sandbox: sbx,
  maxDuration: 300,
  secrets: {
    API_TOKEN: { env: "API_TOKEN" },
  },
  run: async (_payload: unknown, ctx) => {
    if (!process.env.API_TOKEN) {
      throw new Error("API_TOKEN was not injected")
    }
    ctx.log.info({ secret: "API_TOKEN", available: true })
    return { ok: true }
  },
})
