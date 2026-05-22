import { useNavigate } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, For, Show } from "solid-js";
import { ApiError } from "../lib/api";
import {
  connectProjectGitHubRepository,
  listGitHubInstallationRepositories,
  listGitHubInstallations,
  type GitHubRepository,
} from "../lib/github";
import { clearPendingGitHubSetup, readPendingGitHubSetup } from "../lib/github-setup";
import { useScope } from "../lib/scope";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

const GITHUB_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage GitHub connections.",
  not_found: "GitHub repository not found.",
  invalid_github_installation: "The GitHub installation is invalid or no longer available.",
  invalid_github_repository: "Choose a valid GitHub repository.",
  internal: "Something went wrong. Please try again.",
};

function githubErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return GITHUB_ERROR_MESSAGES[error.errorKind] ?? error.message;
  return "Something went wrong. Please try again.";
}

function repositoryConnected(repository: GitHubRepository): boolean {
  return repository.project_github_repository?.connected === true;
}

function repositoryFullName(repository: GitHubRepository): string {
  return repository.full_name ?? repository.name ?? "-";
}

export function GitHubConnectRepositories() {
  const navigate = useNavigate();
  const scope = useScope();
  const queryClient = useQueryClient();
  const pending = readPendingGitHubSetup();
  const [selectedIDs, setSelectedIDs] = createSignal<string[]>([]);
  const [submitting, setSubmitting] = createSignal(false);
  const [initialized, setInitialized] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  createEffect(() => {
    const projectID = pending?.kind === "onboarding" ? pending.project_id : undefined;
    if (!projectID) return;
    if (scope.selectedProjectID() === projectID) return;
    if (!scope.projects().some((project) => project.id === projectID)) return;
    scope.setSelectedProjectID(projectID);
  });

  const installations = createQuery(() => ({
    queryKey: ["github-installations"],
    queryFn: listGitHubInstallations,
    retry: false,
  }));
  const activeInstallations = createMemo(() =>
    (installations.data?.installations ?? []).filter((installation) => installation.status === "active"),
  );
  const installationID = createMemo(() => {
    const pendingInstallationID = pending?.kind === "onboarding" ? pending.installation_id : undefined;
    if (pendingInstallationID && activeInstallations().some((installation) => installation.installation_id === pendingInstallationID)) {
      return pendingInstallationID;
    }
    return activeInstallations()[0]?.installation_id ?? "";
  });
  const installationName = createMemo(() =>
    activeInstallations().find((installation) => installation.installation_id === installationID())?.account_login ?? "GitHub",
  );
  const repositoriesEnabled = createMemo(() => !!installationID() && !!scope.selectedProjectID());

  const repositories = createQuery(() => ({
    queryKey: ["github-connect-repositories", installationID(), scope.selectedProjectID()],
    queryFn: async () => {
      const result = await listGitHubInstallationRepositories(installationID(), {
        project_id: scope.selectedProjectID(),
      });
      return result.repositories.filter((repository) => !repositoryConnected(repository));
    },
    enabled: repositoriesEnabled(),
    retry: false,
  }));

  createEffect(() => {
    if (initialized() || repositories.isPending || repositories.isError) return;
    setSelectedIDs((repositories.data ?? []).map((repository) => repository.github_repository_id));
    setInitialized(true);
  });

  const toggleRepository = (repositoryID: string) => {
    setSelectedIDs((current) =>
      current.includes(repositoryID)
        ? current.filter((id) => id !== repositoryID)
        : [...current, repositoryID],
    );
  };

  const finish = async () => {
    const projectID = scope.selectedProjectID();
    const selected = new Set(selectedIDs());
    const targets = (repositories.data ?? []).filter((repository) => selected.has(repository.github_repository_id));
    if (!projectID || targets.length === 0) {
      clearPendingGitHubSetup();
      navigate("/tasks", { replace: true });
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      for (const repository of targets) {
        await connectProjectGitHubRepository({
          github_repository_id: repository.github_repository_id,
          project_id: projectID,
        });
      }
      clearPendingGitHubSetup();
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["github-installations"] }),
        queryClient.invalidateQueries({ queryKey: ["github-repositories"] }),
      ]);
      navigate("/tasks", { replace: true });
    } catch (e) {
      setError(githubErrorMessage(e));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <AuthScreen>
      <AuthTitle>Choose repositories</AuthTitle>
      <AuthCopy>
        Connect repositories from {installationName()} to {scope.selectedProject()?.name ?? "your project"}.
      </AuthCopy>
      <Show when={!installations.isPending && (!repositoriesEnabled() || !repositories.isPending)} fallback={<p class={ui.muted}>Loading...</p>}>
        <Show
          when={activeInstallations().length > 0}
          fallback={
            <div class={ui.authActions}>
              <p class={ui.fieldError}>Connect GitHub before choosing repositories.</p>
              <button type="button" class={ui.button} onClick={() => navigate("/github/connect")}>
                Connect GitHub
              </button>
            </div>
          }
        >
          <Show
            when={(repositories.data?.length ?? 0) > 0}
            fallback={<p class={ui.emptyState}>No new repositories are available for this project.</p>}
          >
            <div class={"grid max-h-80 gap-1.5 overflow-y-auto pr-1"}>
              <For each={repositories.data ?? []}>
                {(repository) => {
                  const checked = () => selectedIDs().includes(repository.github_repository_id);
                  return (
                    <label class={ui.permissionOption}>
                      <input
                        type="checkbox"
                        checked={checked()}
                        disabled={submitting()}
                        onChange={() => toggleRepository(repository.github_repository_id)}
                      />
                      <span>
                        <strong>{repositoryFullName(repository)}</strong>
                        <span>{repository.private ? "Private" : "Public"} · {repository.default_branch ?? "-"}</span>
                      </span>
                    </label>
                  );
                }}
              </For>
            </div>
          </Show>
          <Show when={error()}>
            <p class={ui.fieldError} role="alert">{error()}</p>
          </Show>
          <div class={ui.authActions}>
            <button type="button" class={ui.button} disabled={submitting()} onClick={finish}>
              {submitting() ? "Connecting..." : selectedIDs().length > 0 ? `Connect selected (${selectedIDs().length})` : "Continue"}
            </button>
          </div>
        </Show>
      </Show>
    </AuthScreen>
  );
}
