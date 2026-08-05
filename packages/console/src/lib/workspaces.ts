import { request } from "./api";

export type WorkspaceSecret = {
  name: string;
  env?: string;
  file?: string;
};

export type Workspace = {
  id: string;
  key?: string;
  sandbox_id: string;
  deployment_id: string;
  status: "available" | "recovery-required" | "deleting";
  secrets: WorkspaceSecret[];
  last_activity_at: string;
  created_at: string;
  updated_at: string;
};

export type WorkspaceScope = {
  projectID: string;
  environmentID: string;
};

export async function getWorkspace(
  id: string,
  scope: WorkspaceScope,
): Promise<Workspace> {
  if (!scope.projectID || !scope.environmentID) {
    throw new Error("Workspace project and environment are required");
  }
  return request<Workspace>(
    `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/workspaces/${encodeURIComponent(id)}`,
  );
}
