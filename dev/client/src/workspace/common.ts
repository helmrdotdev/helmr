import type { HelmrClient, WorkspaceStreamChunk } from "../../../../sdk/typescript/src/index"
import { assert, assertEqual } from "../assert"
import { currentDeploymentURL, type ClientSmokeConfig } from "../config"

export interface WorkspaceRequestScope {
  readonly projectId?: string
  readonly environmentId?: string
}

export interface CurrentDeploymentResponse {
  readonly deployment: {
    readonly id: string
    readonly tasks: readonly {
      readonly task_id: string
    }[]
  } | null
}

export async function currentDeployment(config: ClientSmokeConfig): Promise<CurrentDeploymentResponse["deployment"] & { readonly id: string }> {
  const response = await fetch(currentDeploymentURL(config), {
    headers: { authorization: `Bearer ${config.apiKey}` },
  })
  if (!response.ok) {
    throw new Error(`current deployment request failed: ${response.status} ${await response.text()}`)
  }
  const body = (await response.json()) as CurrentDeploymentResponse
  assert(body.deployment !== null, "no current deployment is available")
  return body.deployment
}

export async function waitForRunningMaterialization(
  client: HelmrClient,
  workspaceId: string,
  scope: WorkspaceRequestScope = {},
  expectedID?: string,
) {
  for (let attempt = 0; attempt < 180; attempt += 1) {
    const materialization = await client.workspaces.connect(workspaceId, scope)
    if (expectedID !== undefined) {
      assertEqual(materialization.id, expectedID, "connect returned a different materialization")
    }
    if (materialization.state === "running") {
      return materialization
    }
    assert(
      materialization.state === "requested" ||
        materialization.state === "materializing" ||
        materialization.state === "restoring",
      `unexpected materialization state ${materialization.state}`,
    )
    await delay(2_000)
  }
  throw new Error(`workspace ${workspaceId} materialization did not reach running`)
}

export async function waitForStreamText(
  read: () => Promise<readonly WorkspaceStreamChunk[]>,
  expected: string,
  label = "stream",
): Promise<string> {
  for (let attempt = 0; attempt < 240; attempt += 1) {
    const text = chunksText(await read())
    if (text.includes(expected)) {
      return text
    }
    await delay(500)
  }
  throw new Error(`${label} did not include ${expected}`)
}

export async function expectStreamFollowUnsupported(stream: AsyncIterable<WorkspaceStreamChunk>): Promise<void> {
  try {
    for await (const _chunk of stream) {
      throw new Error("workspace stream follow yielded a chunk")
    }
  } catch (error) {
    assert(error instanceof Error && error.message === "workspace_stream_follow_unsupported", `unexpected stream follow error: ${String(error)}`)
    return
  }
  throw new Error("workspace stream follow completed without explicit unsupported error")
}

export function chunksText(chunks: readonly WorkspaceStreamChunk[]): string {
  const decoder = new TextDecoder()
  return chunks.map((chunk) => decoder.decode(chunk.data)).join("")
}

export function byteLength(value: string): number {
  return new TextEncoder().encode(value).byteLength
}

export function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}
