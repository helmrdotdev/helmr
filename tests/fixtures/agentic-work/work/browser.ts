import { createServer } from "node:http"
import { mkdtemp, readFile, readdir, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { chromium } from "playwright"
import sharp from "sharp"

const page = `<!doctype html>
<title>Orders</title>
<style>body{font:20px sans-serif;background:#fff} #result{color:#060;background:#cfc;padding:8px}</style>
<h1>Pending orders</h1>
<ul><li data-id="17">Anvil</li><li data-id="23">Rope</li></ul>
<form><input name="customer" placeholder="customer"><select name="priority"><option>normal<option>urgent</select>
<button type="submit">Approve</button></form>
<p id="result"></p>
<script>
document.querySelector("form").addEventListener("submit", async event => {
  event.preventDefault()
  const form = new FormData(event.target)
  const response = await fetch("/approve", { method: "POST", body: JSON.stringify(Object.fromEntries(form)) })
  document.querySelector("#result").textContent = (await response.json()).message
})
</script>`

// Browser processes in this process tree, by command line.
async function browserProcesses(): Promise<string[]> {
  const found: string[] = []
  for (const entry of await readdir("/proc")) {
    if (!/^\d+$/.test(entry)) continue
    const command = await readFile(`/proc/${entry}/cmdline`, "utf8").catch(() => "")
    if (/chrom|headless_shell/i.test(command)) found.push(command.split("\0")[0])
  }
  return found
}

// A browser step: operate a page served from this process only, read what it
// shows, submit its form, capture it, and leave no browser behind.
export async function browserWork() {
  const received: unknown[] = []
  const server = createServer((request, response) => {
    if (request.method === "POST" && request.url === "/approve") {
      let body = ""
      request.on("data", chunk => { body += chunk })
      request.on("end", () => {
        const order = JSON.parse(body)
        received.push(order)
        response.setHeader("content-type", "application/json")
        response.end(JSON.stringify({ message: `approved for ${order.customer} (${order.priority})` }))
      })
      return
    }
    response.setHeader("content-type", "text/html")
    response.end(page)
  })
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve))
  const address = server.address()
  const port = typeof address === "object" && address ? address.port : 0
  const directory = await mkdtemp(join(tmpdir(), "agentic-browser-"))
  // The Program runs inside a guest that is already the isolation boundary and
  // offers no nested user namespaces for Chromium's own sandbox.
  const browser = await chromium.launch({ chromiumSandbox: false })
  try {
    const tab = await browser.newPage({ viewport: { width: 640, height: 480 } })
    await tab.goto(`http://127.0.0.1:${port}/`)
    const heading = await tab.locator("h1").innerText()
    const orders = await tab.locator("li").evaluateAll(items => items.map(item => ({ id: item.getAttribute("data-id"), name: item.textContent })))
    await tab.fill("input[name=customer]", "Wile E.")
    await tab.selectOption("select[name=priority]", "urgent")
    await tab.click("button")
    await tab.locator("#result").filter({ hasText: "approved" }).waitFor()
    const result = await tab.locator("#result").innerText()
    const form = { customer: await tab.inputValue("input[name=customer]"), priority: await tab.inputValue("select[name=priority]") }
    const screenshot = join(directory, "page.png")
    await tab.screenshot({ path: screenshot })
    const image = sharp(await readFile(screenshot))
    const metadata = await image.metadata()
    const stats = await image.stats()
    const running = (await browserProcesses()).length
    await browser.close()
    return {
      browserVersion: browser.version(),
      heading, orders, result, form, received,
      screenshot: { format: metadata.format, width: metadata.width, height: metadata.height, contrast: Math.round(Math.max(...stats.channels.map(channel => channel.stdev))) },
      browserProcessesWhileOpen: running,
      browserProcessesAfterClose: await browserProcesses(),
    }
  } finally {
    await browser.close()
    await new Promise(resolve => server.close(resolve))
    await rm(directory, { recursive: true, force: true })
  }
}
