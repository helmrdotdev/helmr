import { request } from "./api";

export type TaskSourceArtifact = {
  digest: string;
  size_bytes?: number;
  media_type?: string;
};

export type DeploymentStatus = "queued" | "building" | "deployed" | "failed";

export type DeploymentTask = {
  id: string;
  task_id: string;
  file_path?: string;
  export_name?: string;
  handler_entrypoint?: string;
  bundle_digest?: string;
  created_at: string;
};

export type Deployment = {
  id: string;
  project_id: string;
  environment_id: string;
  source_artifact: TaskSourceArtifact;
  build_manifest_digest?: string;
  deployment_manifest_digest?: string;
  runtime_artifact_digest?: string;
  content_hash?: string;
  status: DeploymentStatus;
  tasks: DeploymentTask[];
  created_at: string;
  building_at?: string;
  indexed_at?: string;
  deployed_at?: string;
  failed_at?: string;
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
