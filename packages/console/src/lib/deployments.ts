import { ApiError, request } from "./api";

type DeploymentSourceArtifact = {
  digest: string;
  size_bytes?: number;
  media_type?: string;
};

export type DeploymentStatus = "queued" | "building" | "deployed" | "failed";

export type Deployment = {
  id: string;
  version: string;
  project_id: string;
  environment_id: string;
  content_hash: string;
  deployment_source: DeploymentSourceArtifact;
  status: DeploymentStatus;
  failure?: { code: string; message: string; details: Record<string, unknown> };
  created_at: string;
  building_at?: string;
  built_at?: string;
  deployed_at?: string;
  failed_at?: string;
};

export async function getCurrentDeployment(options: {
  projectID?: string;
  environmentID?: string;
} = {}): Promise<Deployment | null> {
  if (!options.projectID || !options.environmentID) {
    throw new Error("project and environment are required");
  }
  try {
    return await request<Deployment>(
      `/api/projects/${encodeURIComponent(options.projectID)}/environments/${encodeURIComponent(options.environmentID)}/deployments/current`,
    );
  } catch (error) {
    if (error instanceof ApiError && error.code === "no_current_deployment") return null;
    throw error;
  }
}
