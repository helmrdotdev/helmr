import { del, request } from "./api";

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
  return request<ListSecretsResponse>(secretsPath(projectID, environmentID));
}

export async function setSecret(
  name: string,
  value: string,
  projectID: string,
  environmentID: string,
): Promise<Secret> {
  return request<Secret>(`${secretsPath(projectID, environmentID)}/${encodeURIComponent(name)}`, {
    method: "PUT",
    body: JSON.stringify({
      value,
    }),
  });
}

export async function deleteSecret(name: string, projectID: string, environmentID: string): Promise<void> {
  await del<Record<string, never>>(`${secretsPath(projectID, environmentID)}/${encodeURIComponent(name)}`);
}

function secretsPath(projectID: string, environmentID: string): string {
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}/secrets`;
}
