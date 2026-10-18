import { expect, test } from "bun:test"
import { hasUsableDefaultIPv6Route } from "./ipv6-route"

const zero = "00000000000000000000000000000000"
const route = (flags: string, device = "eth0", prefix = "00") =>
  `${zero} ${prefix} ${zero} 00 ${zero} ffffffff 00000001 00000000 ${flags} ${device}`

test("ignores Linux's unreachable loopback default entries", () => {
  expect(hasUsableDefaultIPv6Route(`${route("00200200", "lo")}\n${route("00200200", "lo")}\n`)).toBe(false)
  expect(hasUsableDefaultIPv6Route(route("00200201", "lo"))).toBe(false)
})

test("detects a usable default route even alongside rejected entries", () => {
  expect(hasUsableDefaultIPv6Route(`${route("00200200", "lo")}\n${route("00000003")}`)).toBe(true)
  expect(hasUsableDefaultIPv6Route(route("00000001"))).toBe(true)
})

test("ignores down and non-default routes", () => {
  expect(hasUsableDefaultIPv6Route(route("00000002"))).toBe(false)
  expect(hasUsableDefaultIPv6Route(route("00000001", "eth0", "80"))).toBe(false)
  expect(hasUsableDefaultIPv6Route("\n")).toBe(false)
})
