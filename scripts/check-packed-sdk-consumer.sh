#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

scripts/build-npm-packages.sh

consumer="$(mktemp -d)"
trap 'rm -rf "${consumer}"' EXIT
mkdir -p \
  "${consumer}/node_modules/@helmr/proto" \
  "${consumer}/node_modules/@helmr/sdk" \
  "${consumer}/node_modules/@bufbuild"

proto_archive="$(
  npm pack dist/npm/proto/package --pack-destination "${consumer}" --silent
)"
sdk_archive="$(
  npm pack dist/npm/sdk/package --pack-destination "${consumer}" --silent
)"
tar -xzf "${consumer}/${proto_archive}" \
  --strip-components=1 -C "${consumer}/node_modules/@helmr/proto"
tar -xzf "${consumer}/${sdk_archive}" \
  --strip-components=1 -C "${consumer}/node_modules/@helmr/sdk"
ln -s "${repo_root}/node_modules/@bufbuild/protobuf" \
  "${consumer}/node_modules/@bufbuild/protobuf"

cat >"${consumer}/consumer.ts" <<'EOF'
import {
  HelmrClient,
  actor,
  image,
  queue,
  sandbox,
  source,
  task,
  type Queue,
  type QueueConfig,
  type Actor,
  type ActorStartResult,
  type ImageBuilder,
  type Sandbox,
  type SandboxBuilder,
  type SandboxResourceBuilder,
  type SecretCreateRequest,
  type SessionOutputWriter,
  type SourceDirectory,
  type SourceFile,
  type StandardSchemaV1,
  type TaskConfigWithPayload,
  type TokenCompleteRequest,
  workspaces,
} from "@helmr/sdk"

const payloadSchema: StandardSchemaV1<string, string> = {
  "~standard": {
    version: 1,
    vendor: "packed-consumer",
    validate(value: unknown) {
      return typeof value === "string"
        ? { value }
        : { issues: [{ message: "expected string" }] }
    },
  },
}

const queueConfig: QueueConfig = { name: "packed-consumer", concurrencyLimit: 1 }
const fixtureQueue: Queue = queue(queueConfig)
const fixtureImage: ImageBuilder = image("packed-consumer-image")
  .from("node:24-bookworm-slim")
const sourceFile: SourceFile = source.file("./package.json")
const sourceDirectory: SourceDirectory = source.directory("./src")
const copiedImage = image("copied-source")
  .copy("/app/package.json", sourceFile)
  .copy("/app/src", sourceDirectory)
const stagedSandbox: SandboxBuilder = sandbox({ id: "packed-consumer" })
const resourceSandbox: SandboxResourceBuilder = stagedSandbox.image(
  image("packed-consumer").from("node:24-bookworm-slim"),
)

function outputHelper(writer: SessionOutputWriter): Promise<void> {
  return writer.close()
}

const secretRequest: SecretCreateRequest = { name: "TOKEN", value: "secret" }
const tokenRequest: TokenCompleteRequest = { result: null }
void outputHelper
void secretRequest
void tokenRequest
void copiedImage

const taskConfig = {
  id: "packed-consumer",
  payload: payloadSchema,
  queue: fixtureQueue,
  run: (value) => value,
} satisfies TaskConfigWithPayload<"packed-consumer", string, string, string>
const fixture = task(taskConfig)

const fixtureSandbox = resourceSandbox.resources({ cpu: 1, memory: "1GiB" })
fixtureSandbox satisfies Sandbox
const fixtureActor: Actor = actor({ id: "packed-consumer-actor", run() {} })
function actorStartResultHelper(value: ActorStartResult): string {
  return value.session.id + value.run.id
}
void fixtureImage
void fixtureActor
void actorStartResultHelper

const requests: Array<{ url: string; init?: RequestInit }> = []
const client = new HelmrClient({
  url: "https://example.invalid",
  apiKey: "packed-consumer",
  fetch: async (input: URL | RequestInfo, init?: RequestInit) => {
    requests.push({ url: String(input), init })
    return Response.json({
      run_id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
    })
  },
})
const run = await client.tasks.start<typeof fixture>("packed-consumer", {
  payload: "typed",
  workspace: workspaces.ref("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
  idempotencyKey: "packed-consumer-start",
})
if (run.id !== "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31") {
  throw new Error("packed client did not parse the canonical Run handle")
}
if (requests.length !== 1) {
  throw new Error(`packed client made ${requests.length} requests`)
}
const request = requests[0]
if (request?.url !== "https://example.invalid/v1/tasks/packed-consumer/start") {
  throw new Error(`packed client used an unexpected URL: ${request?.url}`)
}
if (request.init?.method !== "POST") {
  throw new Error("packed client did not serialize a POST request")
}
const body = JSON.parse(String(request.init?.body))
if (
  body.payload !== "typed" ||
  body.workspace?.id !== "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32" ||
  body.idempotency_key !== "packed-consumer-start"
) {
  throw new Error(`packed client serialized an unexpected body: ${JSON.stringify(body)}`)
}
if (
  fixture.id !== "packed-consumer" ||
  fixtureSandbox.id !== "packed-consumer"
) {
  throw new Error("packed definition builders returned an invalid contract")
}
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
