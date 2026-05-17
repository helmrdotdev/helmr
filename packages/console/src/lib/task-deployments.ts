import { request } from "./api";

export type TaskSourceArtifact = {
  digest: string;
  size_bytes?: number;
  media_type?: string;
};

export type DeployedTask = {
  id: string;
  task_id: string;
  module_path?: string;
  export_name?: string;
  created_at: string;
};

export type TaskDeployment = {
  id: string;
  project_id: string;
  environment_id: string;
  source_artifact: TaskSourceArtifact;
  status: string;
  tasks: DeployedTask[];
  created_at: string;
  deployed_at: string;
};

export type GetActiveTaskDeploymentResponse = {
  deployment: TaskDeployment | null;
};

export async function getActiveTaskDeployment(options: {
  projectID?: string;
  environmentID?: string;
} = {}): Promise<GetActiveTaskDeploymentResponse> {
  const params = new URLSearchParams();
  if (options.projectID && options.environmentID) {
    params.set("project_id", options.projectID);
    params.set("environment_id", options.environmentID);
  }
  const query = params.toString();
  return request<GetActiveTaskDeploymentResponse>(`/api/task-deployments/active${query ? `?${query}` : ""}`);
}
