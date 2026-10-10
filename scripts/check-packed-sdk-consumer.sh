#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

consumer="$(mktemp -d)"
trap 'rm -rf "${consumer}"' EXIT
mkdir -p \
  "${consumer}/node_modules/@helmr/proto" \
  "${consumer}/node_modules/@helmr/sdk" \
  "${consumer}/node_modules/@bufbuild"

if [ "$#" -eq 0 ]; then
  scripts/build-npm-packages.sh
  sdk_packages="${consumer}/packages"
  scripts/pack-npm-packages.sh "$sdk_packages"
elif [ "$#" -eq 2 ] && [ "$1" = --sdk-packages ]; then
  sdk_packages="$2"
else
  echo "usage: $0 [--sdk-packages DIR]" >&2
  exit 2
fi
for package in proto sdk; do
  archives=("$sdk_packages"/helmr-"$package"-*.tgz)
  if [ "${#archives[@]}" -ne 1 ] || [ ! -f "${archives[0]}" ]; then
    echo "expected one $package archive in $sdk_packages" >&2
    exit 1
  fi
  tar -xzf "${archives[0]}" \
    --strip-components=1 -C "${consumer}/node_modules/@helmr/${package}"
done
ln -s "${repo_root}/sdk/typescript/node_modules/@bufbuild/protobuf" \
  "${consumer}/node_modules/@bufbuild/protobuf"

cat >"${consumer}/consumer.ts" <<'EOF'
import {
  HelmrClient, agent, computer, image, source,
  type AgentDefinition, type ComputerDefinition, type ImageBuilder, type InputContent,
  type SecretCreateRequest, type SourceDirectory, type SourceFile,
} from "@helmr/sdk"

const sourceFile: SourceFile = source.file("./package.json")
const sourceDirectory: SourceDirectory = source.directory("./src")
const fixtureImage: ImageBuilder = image("packed-consumer-image")
  .from("node:24-bookworm-slim")
  .copy(sourceFile, "/app/package.json")
  .copy(sourceDirectory, "/app/src")
const machine: ComputerDefinition = computer({
  id: "packed-machine", image: fixtureImage, resources: { cpu: 1, memory: "1GiB" },
  prepare: async (build) => { await build.exec(["true"]) },
})
const fixture: AgentDefinition<InputContent, string, { ready: boolean }> = agent<InputContent, string, { ready: boolean }>({
  id: "packed-consumer", computer: machine,
  setup: () => ({ ready: true }),
  turn: async (turn, ctx) => {
    if (!ctx.setupResult.ready) throw new Error("setup result unavailable")
    await turn.output.write("working")
    await turn.respond("done")
    return turn.input.map(part => part.text).join("")
  },
})
const secretRequest: SecretCreateRequest = { name: "TOKEN", value: "secret" }
void secretRequest
if (fixture.kind !== "agent" || machine.kind !== "computer") throw new Error("invalid authored definitions")

const sessionID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33"
const turnID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34"
const computerID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
const requests: string[] = []
const client = new HelmrClient({
  url: "https://example.invalid", apiKey: "packed-consumer",
  fetch: async (input: URL | RequestInfo, init?: RequestInit) => {
    const path = new URL(String(input)).pathname
    requests.push(path)
    if (new Headers(init?.headers).get("Authorization") !== "Bearer packed-consumer") throw new Error("missing client authentication")
    const body = init?.body === undefined ? undefined : JSON.parse(String(init.body))
    if (path === "/v1/agents/packed-consumer/start") {
      if (init?.method !== "POST" || body.input[0]?.type !== "text" || body.input[0]?.text !== "typed" || body.computer_id !== computerID || body.idempotency_key !== "start-1") throw new Error("invalid start envelope")
      return Response.json({ session_id: sessionID, turn_id: turnID, sequence: 1, created: true })
    }
    if (path === `/v1/sessions/${sessionID}/enqueue`) {
      if (body.input[0]?.text !== '{"issue":"APP-42"}' || body.idempotency_key !== "queue-1") throw new Error("invalid enqueue envelope")
      return Response.json({ session_id: sessionID, turn_id: turnID, sequence: 2 })
    }
    if (path === `/v1/sessions/${sessionID}/turns/${turnID}/messages`) {
      if (body.data[0]?.type !== "text" || body.data[0]?.text !== "answer") throw new Error("invalid exact message envelope")
      return Response.json({ id: sessionID, turn_id: turnID, message_id: turnID, status: "accepted" })
    }
    if (path === `/v1/sessions/${sessionID}/events`) {
      return Response.json({ records: [], next_after: 0, has_more: false, retained_after: 0 })
    }
    throw new Error(`unexpected packed client request: ${path}`)
  },
})
const started = await client.agents.start(fixture.id, {
  input: [{ type: "text", text: "typed" }], computer: client.computers.ref(computerID), idempotencyKey: "start-1",
})
if (!started.created || started.session.id !== sessionID || started.turn.id !== turnID) throw new Error("invalid Agent admission")
const session = client.sessions.get(sessionID)
const queued = await session.enqueue([{ type: "text", text: JSON.stringify({ issue: "APP-42" }) }], { idempotencyKey: "queue-1" })
if (queued.id !== turnID) throw new Error("enqueue did not return the exact Turn reference")
const message = await queued.send([{ type: "text", text: "answer" }])
if (message.status !== "accepted") throw new Error("message receipt was not parsed")
const page = await session.events.list({ after: 0, limit: 10 })
if (page.nextAfter !== 0 || page.hasMore) throw new Error("event cursor was not parsed")
if (requests.length !== 4) throw new Error("unexpected request count")
EOF

cat >"${consumer}/package.json" <<'EOF'
{"private":true,"type":"module"}
EOF

cat >"${consumer}/tsconfig.json" <<'EOF'
{
  "compilerOptions": {
    "strict": true,
    "outDir": "dist",
    "target": "ES2022",
    "module": "NodeNext",
    "moduleResolution": "NodeNext",
    "lib": ["ESNext", "DOM"],
    "skipLibCheck": false
  },
  "include": ["consumer.ts"]
}
EOF

"${repo_root}/node_modules/.bin/tsc" -p "${consumer}/tsconfig.json"
(
  cd "${consumer}"
  node --no-warnings dist/consumer.js
)
