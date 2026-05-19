import {
  ApprovalTimeoutError,
  ConcurrentWaitError,
  ImageBuilderImpl,
  MessageTimeoutError,
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
  type Placement,
  type SandboxBuilder,
  type SecretDecls,
  type SecretMountBinding,
  type SourceCapabilities,
  type SourceDirRef,
  type SourceDirectoryOptions,
  type SourceFileRef,
  type TaskContext,
  type TaskOutput,
  type TaskPayload,
  type WorkspaceCapabilities,
  type WorkspaceSpec,
} from "./internal"
import { HelmrClient } from "./runtime/client"
export type {
  WaitpointResponseToken,
  WaitpointTokenAction,
  WaitpointTokenCompleteOptions,
  WaitpointTokenCreateOptions,
} from "./runtime/client"
import { sandbox } from "./sandbox"
import { defineConfig, type HelmrConfig, type HelmrConfigInput } from "./config"
import { task, type Task, type TaskConfig } from "./task"
import { getDefaultClient, tasks } from "./trigger"

export { AuthError, RunNotFoundError, TimeoutError, UnsupportedTransportError } from "./runtime/errors"
export { ApprovalTimeoutError, ConcurrentWaitError, MessageTimeoutError }
export {
  type LogSnapshot,
  type ListRunEventsOptions,
  type ListRunsOptions,
  type PendingApprovalWaitpoint,
  type PendingMessageWaitpoint,
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
  type WaitpointApprovalOptions,
  type WaitpointRef,
  type WaitpointReplyOptions,
} from "./runtime/run"
export { defineConfig, sandbox, task, tasks }
export type {
  HelmrConfig,
  HelmrConfigInput,
  Placement,
  SecretDecls,
  Task,
  TaskConfig,
  TaskOutput,
  TaskPayload,
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
}
