import { expect, test } from "bun:test"
import { accessFetch } from "./access"

const environment = { HELMR_ACCESS_ORIGIN: "https://staging.example.com", HELMR_ACCESS_CLIENT_ID: "id", HELMR_ACCESS_CLIENT_SECRET: "secret" }

test("Access fetch preserves API auth, refuses redirects and restricts its origin", async () => {
  let received: Request | undefined
  const base = (async (input: RequestInfo | URL) => { received = new Request(input); return new Response("{}") }) as typeof fetch
  const secured = accessFetch(environment, base)
  await secured("https://staging.example.com/v1/computers", { headers: { Authorization: "Bearer api" } })
  expect(received!.headers.get("Authorization")).toBe("Bearer api")
  expect(received!.headers.get("CF-Access-Client-Secret")).toBe("secret")
  expect(received!.redirect).toBe("error")
  await expect(secured("https://storage.example.com/upload")).rejects.toThrow("outside")
  await expect(secured("http://staging.example.com/v1/computers")).rejects.toThrow("outside")
})

test("Access fetch leaves unconfigured callers alone and refuses partial config", () => {
  expect(accessFetch({}, fetch)).toBe(fetch)
  expect(() => accessFetch({ HELMR_ACCESS_CLIENT_ID: "id" })).toThrow("together")
  expect(() => accessFetch({ ...environment, HELMR_ACCESS_ORIGIN: "https://user@staging.example.com" })).toThrow("origin")
})
