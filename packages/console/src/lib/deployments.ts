import { request } from "./api";

type DeploymentSourceArtifact = {
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
  version: string;
  project_id: string;
  environment_id: string;
  deployment_source: DeploymentSourceArtifact;
  build_manifest_digest?: string;
  deployment_manifest_digest?: string;
  status: DeploymentStatus;
  tasks: DeploymentTask[];
  created_at: string;
  building_at?: string;
  built_at?: string;
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
  if (!options.projectID || !options.environmentID) {
    throw new Error("project and environment are required");
  }
  return request<GetCurrentDeploymentResponse>(
    `/api/projects/${encodeURIComponent(options.projectID)}/environments/${encodeURIComponent(options.environmentID)}/deployments/current`,
  );
}
