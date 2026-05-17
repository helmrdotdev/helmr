import { request } from "./api";

export type Secret = {
  project_id: string;
  environment_id: string;
  name: string;
  created_at: string;
  updated_at: string;
};

export type ListSecretsResponse = {
  secrets: Secret[];
};

export async function listSecrets(projectID: string, environmentID: string): Promise<ListSecretsResponse> {
  const params = new URLSearchParams({ project_id: projectID, environment_id: environmentID });
  return request<ListSecretsResponse>(`/api/secrets?${params.toString()}`);
}

export async function setSecret(
  name: string,
  value: string,
  projectID: string,
  environmentID: string,
): Promise<Secret> {
  return request<Secret>(`/api/secrets/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify({
      project_id: projectID,
      environment_id: environmentID,
      value,
    }),
  });
}
