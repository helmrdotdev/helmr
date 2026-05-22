const GITHUB_SETUP_STORAGE_KEY = "helmr.github_setup";
const GITHUB_SETUP_MAX_AGE_MS = 30 * 60 * 1000;

export type GitHubSetupKind = "onboarding" | "settings";

export type PendingGitHubSetup = {
  kind: GitHubSetupKind;
  installation_id?: string;
  project_id?: string;
  created_at: number;
};

function parsePendingGitHubSetup(value: string | null): PendingGitHubSetup | null {
  if (!value) return null;
  const parsed: unknown = JSON.parse(value);
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return null;
  const setup = parsed as Partial<PendingGitHubSetup>;
  const kind = setup.kind === "onboarding" || setup.kind === "settings" ? setup.kind : "settings";
  if (typeof setup.created_at !== "number") return null;
  if (Date.now() - setup.created_at > GITHUB_SETUP_MAX_AGE_MS) {
    clearPendingGitHubSetup();
    return null;
  }
  const pending: PendingGitHubSetup = {
    kind,
    created_at: setup.created_at,
  };
  if (typeof setup.installation_id === "string") pending.installation_id = setup.installation_id;
  if (typeof setup.project_id === "string") pending.project_id = setup.project_id;
  return pending;
}

export function readPendingGitHubSetup(): PendingGitHubSetup | null {
  try {
    return parsePendingGitHubSetup(sessionStorage.getItem(GITHUB_SETUP_STORAGE_KEY));
  } catch {
    return null;
  }
}

export function rememberGitHubSetup(input: {
  kind: GitHubSetupKind;
  project_id?: string;
  installation_id?: string;
}) {
  try {
    const current = readPendingGitHubSetup();
    const next: PendingGitHubSetup = {
      kind: input.kind,
      created_at: Date.now(),
    };
    const projectID = input.project_id ?? current?.project_id;
    const installationID = input.installation_id ?? current?.installation_id;
    if (projectID) next.project_id = projectID;
    if (installationID) next.installation_id = installationID;
    sessionStorage.setItem(GITHUB_SETUP_STORAGE_KEY, JSON.stringify(next));
  } catch {
    // GitHub setup can still continue without the post-install prompt state.
  }
}

export function clearPendingGitHubSetup() {
  try {
    sessionStorage.removeItem(GITHUB_SETUP_STORAGE_KEY);
  } catch {
    // Nothing to clear.
  }
}
