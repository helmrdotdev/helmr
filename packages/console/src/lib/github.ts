import { postJson, request } from "./api";

export type GitHubInstallationStatus = "active" | "suspended" | "deleted";

export type GitHubInstallation = {
  installation_id: string;
  account_login: string;
  account_type: string;
  repository_selection?: string;
  status: GitHubInstallationStatus;
  html_url?: string;
  created_at: string;
  updated_at: string;
};

export type GitHubInstallationsResponse = {
  install_url: string;
  installations: GitHubInstallation[];
};

export type GitHubProjectRepository = {
  project_id: string;
  connected: boolean;
};

export type GitHubRepository = {
  github_repository_id: string;
  installation_id: string;
  owner_login: string;
  name: string;
  full_name?: string;
  private?: boolean;
  archived?: boolean;
  default_branch?: string;
  status: string;
  html_url?: string;
  project_github_repository?: GitHubProjectRepository | null;
  updated_at?: string;
};

export type GitHubRepositoriesResponse = {
  repositories: GitHubRepository[];
  has_more?: boolean;
};

export type GitHubProjectRepositoryInput = {
  github_repository_id: string;
  project_id: string;
};

export async function listGitHubInstallations(): Promise<GitHubInstallationsResponse> {
  return request<GitHubInstallationsResponse>("/api/github/installations");
}

export async function listGitHubInstallationRepositories(
  installationID: string,
  scope?: { project_id: string },
): Promise<GitHubRepositoriesResponse> {
  const params = new URLSearchParams();
  if (scope?.project_id) params.set("project_id", scope.project_id);
  const query = params.toString();
  return request<GitHubRepositoriesResponse>(
    `/api/github/installations/${encodeURIComponent(installationID)}/repositories${query ? `?${query}` : ""}`,
  );
}

export async function connectProjectGitHubRepository(
  input: GitHubProjectRepositoryInput,
): Promise<GitHubRepository> {
  return request<GitHubRepository>(
    `/api/projects/${encodeURIComponent(input.project_id)}/github/repositories/${encodeURIComponent(input.github_repository_id)}`,
    { method: "PUT" },
  );
}

export async function disconnectProjectGitHubRepository(
  input: GitHubProjectRepositoryInput,
): Promise<GitHubRepository> {
  return request<GitHubRepository>(
    `/api/projects/${encodeURIComponent(input.project_id)}/github/repositories/${encodeURIComponent(input.github_repository_id)}`,
    { method: "DELETE" },
  );
}

export async function startGitHubSetup(input: {
  installation_id: string;
  setup_action?: string;
}): Promise<{ redirect_url: string }> {
  return postJson<
    { installation_id: string; setup_action?: string },
    { redirect_url: string }
  >("/api/github/setup/start", input);
}
