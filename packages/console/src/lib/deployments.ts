import { request } from "./api";

export type TaskSourceArtifact = {
  digest: string;
  size_bytes?: number;
  media_type?: string;
};

export type DeploymentTask = {
  id: string;
  task_id: string;
  module_path?: string;
  export_name?: string;
  created_at: string;
};

export type Deployment = {
  id: string;
  project_id: string;
  environment_id: string;
  source_artifact: TaskSourceArtifact;
  status: string;
  tasks: DeploymentTask[];
  created_at: string;
  deployed_at: string;
};

export type GetCurrentDeploymentResponse = {
  deployment: Deployment | null;
};

export async function getCurrentDeployment(options: {
  projectID?: string;
  environmentID?: string;
} = {}): Promise<GetCurrentDeploymentResponse> {
  const params = new URLSearchParams();
  if (options.projectID && options.environmentID) {
    params.set("project_id", options.projectID);
    params.set("environment_id", options.environmentID);
  }
  const query = params.toString();
  return request<GetCurrentDeploymentResponse>(`/api/deployments/current${query ? `?${query}` : ""}`);
}
