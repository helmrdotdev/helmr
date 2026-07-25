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
  image,
  task,
  type StandardSchemaV1,
  workspace,
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

const fixture = task({
  id: "packed-consumer",
  payload: payloadSchema,
  run: (value) => value,
})

const fixtureWorkspace = workspace("packed-consumer")
  .image(image("packed-consumer").from("node:24-bookworm-slim"))
  .resources({ cpu: 1, memory: "1GiB" })

const requests: Array<{ url: string; init?: RequestInit }> = []
const client = new HelmrClient({
  url: "https://example.invalid",
  apiKey: "packed-consumer",
  fetch: async (input: URL | RequestInfo, init?: RequestInit) => {
    requests.push({ url: String(input), init })
    return Response.json({
      run_id: "run_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    })
  },
})
const run = await client.tasks.start<typeof fixture>("packed-consumer", {
  payload: "typed",
  workspace: workspaces.ref({ key: "packed-consumer" }),
  idempotencyKey: "packed-consumer-start",
})
if (run.id !== "run_aaaaaaaaaaaaaaaaaaaaaaaaaa") {
  throw new Error("packed client did not parse the canonical Run handle")
}
if (requests.length !== 1) {
  throw new Error(`packed client made ${requests.length} requests`)
}
const request = requests[0]
if (request?.url !== "https://example.invalid/api/tasks/packed-consumer/start") {
  throw new Error(`packed client used an unexpected URL: ${request?.url}`)
}
if (request.init?.method !== "POST") {
  throw new Error("packed client did not serialize a POST request")
}
const body = JSON.parse(String(request.init?.body))
if (
  body.payload !== "typed" ||
  body.workspace?.key !== "packed-consumer" ||
  body.idempotency_key !== "packed-consumer-start"
) {
  throw new Error(`packed client serialized an unexpected body: ${JSON.stringify(body)}`)
}
if (
  fixture.id !== "packed-consumer" ||
  fixtureWorkspace.id !== "packed-consumer"
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
