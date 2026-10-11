import { image, computer, agent } from "@helmr/sdk"
import { browserWork } from "../work/browser.ts"
import { gitWork } from "../work/git.ts"
import { imageWork } from "../work/image.ts"
import { processWork } from "../work/process.ts"
import { pythonWork } from "../work/python.ts"

// The Computer image owns everything Program code spawns: the Chromium build
// matching the installed playwright package with its OS libraries, Git, and a
// Python environment with NumPy. None of it comes from the build environment.
const tools = [
  "apt-get update",
  "apt-get install -y --no-install-recommends git python3-venv",
  "rm -rf /var/lib/apt/lists/*",
  "python3 -m venv /opt/agentic-python",
  "/opt/agentic-python/bin/pip install --no-cache-dir numpy==2.5.3",
].join(" && ")

const agenticImage = image("agentic-work")
  .from("mcr.microsoft.com/playwright:v1.63.0-noble@sha256:eff16c30e6f3f4af0a03fa4b706120d5e9b0891c344a27d64559aff5900a4a27")
  .run(["sh", "-ceu", tools])

export const agenticComputer = computer({ id: "agentic-work", image: agenticImage, resources: { cpu: 2, memory: "2GiB" } })

export const gitEdit = agent({ id: "git-edit", computer: agenticComputer, maxTurnDuration: "5m", turn: () => gitWork() })
export const browserPage = agent({ id: "browser-page", computer: agenticComputer, maxTurnDuration: "5m", turn: () => browserWork() })
export const pythonData = agent({ id: "python-data", computer: agenticComputer, maxTurnDuration: "5m", turn: () => pythonWork() })
export const imageTransform = agent({ id: "image-transform", computer: agenticComputer, maxTurnDuration: "5m", turn: () => imageWork() })
export const toolProcess = agent({ id: "tool-process", computer: agenticComputer, maxTurnDuration: "5m", turn: () => processWork() })
