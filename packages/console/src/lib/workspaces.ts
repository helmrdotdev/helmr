import { postJson, request } from "./api";

export type WorkspaceSecret = {
  name: string;
  env?: string;
  file?: string;
};

export type WorkspaceStatus = "available" | "recovery_required" | "deleting";

export type Workspace = {
  id: string;
  key?: string;
  sandbox_id: string;
  deployment_id: string;
  status: WorkspaceStatus;
  secrets: WorkspaceSecret[];
  // Present when a Session or Run owns the Workspace; omitted when unowned.
  owner?: { session_id?: string; run_id?: string };
  last_activity_at: string;
  created_at: string;
  updated_at: string;
};

export type WorkspaceListItem = Omit<Workspace, "secrets">;

export type ListWorkspacesResponse = {
  workspaces: WorkspaceListItem[];
  next_cursor?: string;
};

export type WorkspaceScope = {
  projectID: string;
  environmentID: string;
};

export type CreateWorkspaceInput = {
  key?: string;
  secrets?: WorkspaceSecret[];
  idempotency_key: string;
};

export type WorkspaceExecStatus = "pending" | "running" | "exited" | "failed";

export type WorkspaceExecProcess = {
  process_id: string;
  status: WorkspaceExecStatus;
  exit_code?: number;
  stdout_base64?: string;
  stderr_base64?: string;
  error?: { terminal_reason_code: string };
};

export type ExecWorkspaceInput = {
  command: string[];
  cwd?: string;
  timeout?: string;
  idempotency_key: string;
};

export async function listWorkspaces(
  scope: WorkspaceScope,
  options: { cursor?: string | undefined; limit?: number | undefined } = {},
): Promise<ListWorkspacesResponse> {
  const params = new URLSearchParams();
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.size === 0 ? "" : `?${params.toString()}`;
  return request<ListWorkspacesResponse>(`${environmentPath(scope)}/workspaces${query}`);
}

export async function getWorkspace(id: string, scope: WorkspaceScope): Promise<Workspace> {
  return request<Workspace>(workspacePath(id, scope));
}

export async function createWorkspace(
  sandboxID: string,
  scope: WorkspaceScope,
  input: CreateWorkspaceInput,
): Promise<Workspace> {
  return postJson<CreateWorkspaceInput, Workspace>(
    `${environmentPath(scope)}/sandboxes/${encodeURIComponent(sandboxID)}/workspaces`,
    input,
  );
}

export async function deleteWorkspace(
  id: string,
  scope: WorkspaceScope,
  input: { idempotency_key: string },
): Promise<{ workspace_id: string }> {
  return request<{ workspace_id: string }>(workspacePath(id, scope), {
    method: "DELETE",
    body: JSON.stringify(input),
  });
}

export async function execWorkspace(
  id: string,
  scope: WorkspaceScope,
  input: ExecWorkspaceInput,
): Promise<WorkspaceExecProcess> {
  return postJson<ExecWorkspaceInput, WorkspaceExecProcess>(`${workspacePath(id, scope)}/exec`, input);
}

export async function getWorkspaceExec(
  id: string,
  processID: string,
  scope: WorkspaceScope,
): Promise<WorkspaceExecProcess> {
  return request<WorkspaceExecProcess>(`${workspacePath(id, scope)}/exec/${encodeURIComponent(processID)}`);
}

export function isTerminalExecStatus(status: WorkspaceExecStatus): boolean {
  return status === "exited" || status === "failed";
}

// The exec form takes argv on one line; arguments are separated by whitespace.
export function parseExecCommand(line: string): string[] {
  return line.split(/\s+/).filter((part) => part !== "");
}

export function decodeExecOutput(value: string | undefined): string {
  if (!value) return "";
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return new TextDecoder().decode(bytes);
}

function workspacePath(id: string, scope: WorkspaceScope): string {
  return `${environmentPath(scope)}/workspaces/${encodeURIComponent(id)}`;
}

function environmentPath(scope: WorkspaceScope): string {
  if (!scope.projectID || !scope.environmentID) {
    throw new Error("Workspace project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}`;
}
