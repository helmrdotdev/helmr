export function hasUsableDefaultIPv6Route(routes: string): boolean {
  return routes.split("\n").some((line) => {
    const fields = line.trim().split(/\s+/)
    const flags = Number.parseInt(fields[8] ?? "", 16)
    // Linux also exposes an unreachable ::/0 entry on lo. It cannot carry traffic.
    const up = (flags & 0x1) !== 0
    const reject = (flags & 0x200) !== 0
    return fields[0] === "00000000000000000000000000000000" &&
      fields[1] === "00" && up && !reject
  })
}
