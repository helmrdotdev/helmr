import { image, task, sandbox } from "@helmr/sdk"
import { readFile } from "node:fs/promises"

const base = image("helmr-network-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")

export const networkSmokeWorkspace = sandbox({ id: "helmr-network-smoke" })
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

export const networkSmoke = task({
  id: "network-smoke",
  maxDuration: "2m",
  retry: { enabled: false },
  run: async () => {
    const publicResponse = await fetch("https://checkip.amazonaws.com", {
      signal: AbortSignal.timeout(10_000),
    })
    if (!publicResponse.ok) {
      throw new Error(`public IPv4 probe returned ${publicResponse.status}`)
    }
    const publicAddress = (await publicResponse.text()).trim()
    if (!/^(?:\d{1,3}\.){3}\d{1,3}$/.test(publicAddress)) {
      throw new Error("public IPv4 probe did not return an IPv4 address")
    }

    const ipv6Routes = await readFile("/proc/net/ipv6_route", "utf8")
    const hasDefaultIPv6Route = ipv6Routes
      .split("\n")
      .filter((line) => line.trim() !== "")
      .some((line) => {
        const fields = line.trim().split(/\s+/)
        return fields[0] === "00000000000000000000000000000000" &&
          fields[1] === "00"
      })
    if (hasDefaultIPv6Route) {
      throw new Error("guest unexpectedly has an IPv6 default route")
    }

    return {
      publicIPv4: true,
      ipv6DefaultRoute: false,
    }
  },
})
