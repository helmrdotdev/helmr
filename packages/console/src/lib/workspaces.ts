import { postJson, request } from "./api";

export type Workspace = {
  id: string;
  project_id: string;
  environment_id: string;
  sandbox_id: string;
  external_id?: string;
  current_version_id?: string;
  state: string;
  desired_state: string;
  dirty_state: string;
  metadata?: unknown;
  tags?: string[];
  last_activity_at: string;
  created_at: string;
  updated_at: string;
  archived_at?: string;
  deleted_at?: string;
};

export type WorkspaceEnvelope = {
  workspace: Workspace;
  is_cached?: boolean;
};

export type WorkspaceExec = {
  id: string;
  workspace_id: string;
  command: unknown;
  cwd: string;
  env_shape?: unknown;
  filesystem_mode: string;
  state: string;
  detached: boolean;
  exit_code?: number;
  signal?: string;
  error?: unknown;
  stdout_cursor: number;
  stderr_cursor: number;
  stdin_cursor: number;
  stdin_closed_at?: string;
  created_at: string;
  started_at?: string;
  exited_at?: string;
  updated_at: string;
};

export type WorkspaceExecEnvelope = {
  exec: WorkspaceExec;
  is_cached?: boolean;
};

export type ListWorkspaceExecsResponse = {
  execs: WorkspaceExec[];
};

export type WorkspaceExecStreamChunk = {
  id: string;
  stream: string;
  offset_start: number;
  offset_end: number;
  data: string;
  observed_at: string;
  created_at: string;
};

export type ListWorkspaceExecStreamChunksResponse = {
  chunks: WorkspaceExecStreamChunk[];
};

export type WorkspaceScope = {
  projectID: string;
  environmentID: string;
};

export type WorkspaceMount = {
  id: string;
  workspace_id: string;
  state: string;
};

export type WorkspaceStopResponse = {
  workspace_id: string;
  state: string;
  mount?: WorkspaceMount;
};

export async function getWorkspace(id: string): Promise<WorkspaceEnvelope> {
  return request<WorkspaceEnvelope>(`/api/workspaces/${encodeURIComponent(id)}`);
}

export async function listWorkspaceExecs(
  workspaceID: string,
  scope: WorkspaceScope,
): Promise<ListWorkspaceExecsResponse> {
  return request<ListWorkspaceExecsResponse>(`${workspacePath(workspaceID, scope)}/execs`);
}

export async function getWorkspaceExec(
  workspaceID: string,
  execID: string,
  scope: WorkspaceScope,
): Promise<WorkspaceExecEnvelope> {
  return request<WorkspaceExecEnvelope>(
    `${workspacePath(workspaceID, scope)}/execs/${encodeURIComponent(execID)}`,
  );
}

export async function listWorkspaceExecOutput(
  workspaceID: string,
  execID: string,
  stream: "stdout" | "stderr",
  scope: WorkspaceScope,
): Promise<ListWorkspaceExecStreamChunksResponse> {
  return request<ListWorkspaceExecStreamChunksResponse>(
    `${workspacePath(workspaceID, scope)}/execs/${encodeURIComponent(execID)}/${stream}`,
  );
}

export async function materializeWorkspace(
  workspaceID: string,
  scope: WorkspaceScope,
): Promise<WorkspaceMount> {
  return postJson<Record<string, never>, WorkspaceMount>(
    `${workspacePath(workspaceID, scope)}/materialize`,
    {},
  );
}

export async function stopWorkspace(
  workspaceID: string,
  scope: WorkspaceScope,
): Promise<WorkspaceStopResponse> {
  return postJson<Record<string, never>, WorkspaceStopResponse>(
    `${workspacePath(workspaceID, scope)}/stop`,
    {},
  );
}

function workspacePath(workspaceID: string, scope: WorkspaceScope): string {
  if (!scope.projectID || !scope.environmentID) {
    throw new Error("project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/workspaces/${encodeURIComponent(workspaceID)}`;
}
