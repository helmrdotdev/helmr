import {
  ConcurrentWaitError,
  ImageBuilderImpl,
  SourceDirRefImpl,
  SourceFileRefImpl,
  validateSecretName,
  type CacheBuilder,
  type CacheMount,
  type CacheMountBinding,
  type EmitContent,
  type EmitEvent,
  type ImageBuilder,
  type ImageRunOptions,
  type NoPayload,
  type Placement,
  type SandboxBuilder,
  type SecretDecls,
  type SecretMountBinding,
  type SourceCapabilities,
  type SourceDirRef,
  type SourceDirectoryOptions,
  type SourceFileRef,
  type TaskContext,
  type TaskSource,
  type TaskWorkspace,
  type GitHubRefKind,
  type GitHubPullRequestMetadata,
  type GitHubTaskSource,
  type TaskOutput,
  type TaskPayload,
  type TaskTriggerPayload,
  type WaitCapabilities,
  type WaitForInput,
  type WaitJson,
  type WaitOptions,
  type WaitResolution,
  type WaitManualOptions,
  type WaitUntilInput,
  type WorkspaceCapabilities,
  type WorkspaceSpec,
} from "./internal"
import { PayloadSchemaValidationError } from "./schema/payload"
import { idempotencyKeys } from "./idempotency"
export type { IdempotencyKey, IdempotencyKeyCreateOptions, IdempotencyKeyInput, IdempotencyKeyScope } from "./idempotency"
import type { PayloadSchema, StandardSchemaV1 } from "./schema/payload"
import { HelmrClient } from "./runtime/client"
export type {
  WaitpointResponseToken,
  WaitpointTokenRespondOptions,
  WaitpointTokenCreateOptions,
} from "./runtime/client"
import { sandbox } from "./sandbox"
import { defineConfig, type HelmrConfig, type HelmrConfigInput } from "./config"
import { queue, task, type Task, type TaskConfig, type TaskQueueConfig } from "./task"
import { getDefaultClient, tasks } from "./trigger"

export { AuthError, RunNotFoundError, TimeoutError, UnsupportedTransportError } from "./runtime/errors"
export { ConcurrentWaitError, PayloadSchemaValidationError }
export {
  type LogSnapshot,
  type ListRunEventsOptions,
  type ListRunsOptions,
  type PendingDelayWaitpoint,
  type PendingManualWaitpoint,
  type PendingWaitpoint,
  type RetrieveRunOptions,
  type RunEvent,
  type RunEventRecord,
  type RunEventRecordPage,
  type RunHandle,
  type RunSnapshot,
  type RunStatus,
  type RunSummary,
  type RunWaitOptions,
  type SubscribeRunEventsOptions,
  type WaitpointRespondOptions,
  type WaitpointRef,
} from "./runtime/run"
export { defineConfig, idempotencyKeys, queue, sandbox, task, tasks }
export type {
  HelmrConfig,
  HelmrConfigInput,
  Placement,
  SecretDecls,
  Task,
  TaskConfig,
  TaskQueueConfig,
  TaskOutput,
  TaskPayload,
  TaskTriggerPayload,
  WaitCapabilities,
  WaitForInput,
  WaitJson,
  WaitOptions,
  WaitResolution,
  WaitManualOptions,
  WaitUntilInput,
  PayloadSchema,
  StandardSchemaV1,
  NoPayload,
}

export const image = (id: string): ImageBuilder => new ImageBuilderImpl(id)

export const cache = (id: string): CacheBuilder => ({ id })

export const source: SourceCapabilities = {
  file(path: string): SourceFileRef {
    return new SourceFileRefImpl(path)
  },
  directory(path: string, opts?: SourceDirectoryOptions): SourceDirRef {
    return new SourceDirRefImpl(path, opts?.ignore ? [...opts.ignore] : [])
  },
}

export const workspace: WorkspaceCapabilities = {
  github(
    repo: string,
    opts: {
      readonly ref: string
      readonly subpath?: string
    },
  ): WorkspaceSpec {
    const [org, name, extra] = repo.split("/")
    if (!org || !name || extra !== undefined) {
      throw new Error('workspace.github() repo must be "org/repo"')
    }
    const ref = opts.ref.trim()
    if (!ref) {
      throw new Error("workspace.github() ref is required")
    }
    if (ref.includes("\0")) {
      throw new Error("workspace.github() ref must not contain NUL")
    }
    return {
      kind: "github",
      repository: repo,
      ref,
      ...(opts.subpath === undefined ? {} : { subpath: opts.subpath }),
    }
  },
}

export { HelmrClient }
export { validateSecretName }
export const runs = new Proxy({} as HelmrClient["runs"], {
  get(_target, property, receiver) {
    return Reflect.get(getDefaultClient().runs, property, receiver)
  },
})

export const waitpoints = new Proxy({} as HelmrClient["waitpoints"], {
  get(_target, property, receiver) {
    return Reflect.get(getDefaultClient().waitpoints, property, receiver)
  },
})

export type {
  CacheBuilder,
  CacheMount,
  CacheMountBinding,
  EmitContent,
  EmitEvent,
  ImageBuilder,
  ImageRunOptions,
  SandboxBuilder,
  SecretMountBinding,
  SourceCapabilities,
  SourceDirRef,
  SourceDirectoryOptions,
  SourceFileRef,
  WorkspaceCapabilities,
  WorkspaceSpec,
  TaskContext,
  TaskSource,
  TaskWorkspace,
  GitHubRefKind,
  GitHubPullRequestMetadata,
  GitHubTaskSource,
}
