import { ApiError, request } from "./api";

export type Deployment = {
  id: string;
  version: string;
  bundle_digest: string;
  created_at: string;
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
