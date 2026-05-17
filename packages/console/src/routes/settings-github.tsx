import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { ApiError } from "../lib/api";
import {
  disableProjectWorkspaceRepository,
  disableGitHubRepositoryConnection,
  enableGitHubRepositoryConnection,
  enableProjectWorkspaceRepository,
  listGitHubInstallationRepositories,
  listGitHubInstallations,
  type GitHubInstallation,
  type GitHubRepository,
} from "../lib/github";
import { useScope } from "../lib/scope";
import { cx, statusBadgeClass, ui } from "../ui/styles";
import { createMemo, createSignal, For, Show } from "solid-js";

const STATUS_LABELS: Record<GitHubInstallation["status"], string> = {
  active: "Active",
  suspended: "Suspended",
  deleted: "Deleted",
};

const GITHUB_ERROR_MESSAGES: Record<string, string> = {
  forbidden: "You do not have permission to manage GitHub connections.",
  not_found: "GitHub repository not found.",
  invalid_github_installation: "The GitHub installation is invalid or no longer available.",
  invalid_github_repository: "Choose a valid GitHub repository.",
  internal: "Something went wrong. Please try again.",
};
const INTERNAL_ERROR_MESSAGE = "Something went wrong. Please try again.";

type RepositoryAction = "connect" | "disconnect" | "enable" | "disable";

function selectionLabel(value?: string): string {
  if (value === "all") return "All repositories";
  if (value === "selected") return "Selected repositories";
  return "Unknown";
}

function formatDate(value?: string | null): string {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "-";
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

function githubErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    return GITHUB_ERROR_MESSAGES[error.errorKind] ?? error.message ?? INTERNAL_ERROR_MESSAGE;
  }
  return INTERNAL_ERROR_MESSAGE;
}

function repositoryConnectionEnabled(repository: GitHubRepository): boolean {
  return repository.access_enabled;
}

function repositoryConnected(repository: GitHubRepository): boolean {
  return repository.project_workspace_repository?.enabled === true;
}

function repositoryFullName(repository: GitHubRepository): string {
  return repository.full_name ?? repository.name ?? "-";
}

function repositoryScopeText(repository: GitHubRepository): string {
  const connection = repository.project_workspace_repository;
  if (!connection?.enabled) return "Not allowed";
  return connection.project_id.slice(0, 8);
}

function StatusBadge(props: { status: GitHubInstallation["status"] }) {
  const tone = () => props.status === "active" ? "succeeded" : "revoked";
  return (
    <span class={statusBadgeClass(tone())}>
      {STATUS_LABELS[props.status]}
    </span>
  );
}

function AccessStatusBadge(props: { enabled: boolean }) {
  const label = () => props.enabled ? "Access enabled" : "Access disabled";
  const tone = (): "succeeded" | "revoked" => {
    return props.enabled ? "succeeded" : "revoked";
  };
  return <span class={statusBadgeClass(tone())}>{label()}</span>;
}

function GitHubInstallationRow(props: { installation: GitHubInstallation }) {
  return (
    <tr class={ui.detailTableRow}>
      <td>
        <div class={ui.tableCellStack}>
          <strong>{props.installation.account_login}</strong>
          <div class={ui.muted}>{props.installation.account_type}</div>
        </div>
      </td>
      <td>{selectionLabel(props.installation.repository_selection)}</td>
      <td><StatusBadge status={props.installation.status} /></td>
      <td>{formatDate(props.installation.updated_at)}</td>
      <td class={ui.actionsCell}>
        <Show
          when={props.installation.html_url}
          fallback={<span class={ui.muted}>No actions</span>}
        >
          {(url) => (
            <a class={ui.secondaryButton} href={url()} rel="noreferrer" target="_blank">
              Configure
            </a>
          )}
        </Show>
      </td>
    </tr>
  );
}

function RepositoryRow(props: {
  repository: GitHubRepository;
  accountLogin: string;
  action: { repository: string; action: RepositoryAction } | null;
  error: string | null;
  onAction: (repository: GitHubRepository, action: RepositoryAction) => void;
}) {
  const connected = () => repositoryConnected(props.repository);
  const enabled = () => repositoryConnectionEnabled(props.repository);
  const fullName = () => repositoryFullName(props.repository);
  const busy = (action: RepositoryAction) =>
    props.action?.repository === fullName() && props.action.action === action;
  return (
    <tr class={ui.detailTableRow}>
      <td>
        <div class={ui.tableCellStack}>
          <strong>{fullName()}</strong>
          <div class={ui.muted}>{props.repository.private ? "Private" : "Public"}</div>
        </div>
      </td>
      <td>{props.accountLogin}</td>
      <td><AccessStatusBadge enabled={enabled()} /></td>
      <td>{repositoryScopeText(props.repository)}</td>
      <td><code>{props.repository.default_branch ?? "-"}</code></td>
      <td>{formatDate(props.repository.updated_at)}</td>
      <td class={ui.actionsCell}>
        <div class={"flex flex-wrap items-center gap-1.5"}>
          <Show when={props.repository.html_url}>
            {(url) => (
              <a class={ui.ghostButton} href={url()} rel="noreferrer" target="_blank">
                Open
              </a>
            )}
          </Show>
          <Show when={connected()}>
            <button
              type="button"
              class={ui.secondaryButton}
              disabled={busy("disconnect")}
              onClick={() => props.onAction(props.repository, "disconnect")}
            >
              {busy("disconnect") ? "Removing..." : "Remove from project"}
            </button>
          </Show>
          <Show
            when={!enabled()}
            fallback={
              <>
                <Show when={!connected()}>
                  <button
                    type="button"
                    class={ui.button}
                    disabled={busy("connect")}
                    onClick={() => props.onAction(props.repository, "connect")}
                  >
                    {busy("connect") ? "Allowing..." : "Allow for runs"}
                  </button>
                </Show>
                <button
                  type="button"
                  class={ui.ghostButton}
                  disabled={busy("disable")}
                  onClick={() => props.onAction(props.repository, "disable")}
                >
                  {busy("disable") ? "Disabling..." : "Disable access"}
                </button>
              </>
            }
          >
            <button
              type="button"
              class={ui.secondaryButton}
              disabled={busy("enable")}
              onClick={() => props.onAction(props.repository, "enable")}
            >
              {busy("enable") ? "Enabling..." : "Enable"}
            </button>
          </Show>
        </div>
        <Show when={props.error}>
          <p class={ui.rowError} role="alert">{props.error}</p>
        </Show>
      </td>
    </tr>
  );
}

export function SettingsGitHub() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const [repositoryAction, setRepositoryAction] = createSignal<{ repository: string; action: RepositoryAction } | null>(null);
  const [repositoryError, setRepositoryError] = createSignal<{ repository: string; message: string } | null>(null);

  const installations = createQuery(() => ({
    queryKey: ["github-installations"],
    queryFn: listGitHubInstallations,
    retry: false,
  }));

  const activeInstallations = createMemo(() =>
    (installations.data?.installations ?? []).filter((installation) => installation.status === "active"),
  );
  const installationIDs = createMemo(() =>
    activeInstallations().map((installation) => installation.installation_id).join(","),
  );
  const installationAccountByID = createMemo(() => {
    const accounts: Record<string, string> = {};
    for (const installation of installations.data?.installations ?? []) {
      accounts[installation.installation_id] = installation.account_login;
    }
    return accounts;
  });

  const repositories = createQuery(() => ({
    queryKey: ["github-repositories", installationIDs(), scope.selectedProjectID()],
    queryFn: async () => {
      const selectedScope = {
        project_id: scope.selectedProjectID(),
      };
      const items = await Promise.all(
        activeInstallations().map(async (installation) => {
          const result = await listGitHubInstallationRepositories(installation.installation_id, selectedScope);
          return result.repositories.map((repository) => ({
            ...repository,
            installation_id: repository.installation_id || installation.installation_id,
          }));
        }),
      );
      return items.flat().sort((left, right) => repositoryFullName(left).localeCompare(repositoryFullName(right)));
    },
    enabled: !installations.isPending && !installations.isError && activeInstallations().length > 0 && !!scope.selectedProjectID(),
    retry: false,
  }));

  const install = () => {
    const url = installations.data?.install_url;
    if (url) window.location.href = url;
  };

  const invalidateGitHub = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["github-installations"] }),
      queryClient.invalidateQueries({ queryKey: ["github-repositories"] }),
    ]);
  };

  const updateRepository = async (repository: GitHubRepository, action: RepositoryAction) => {
    if (!scope.selectedProjectID()) return;
    const fullName = repositoryFullName(repository);
    if (fullName === "-") return;
    setRepositoryError(null);
    setRepositoryAction({ repository: fullName, action });
    try {
      const base = {
        installation_id: repository.installation_id,
        github_repository_id: repository.github_repository_id,
        project_id: scope.selectedProjectID(),
      };
      if (action === "connect") {
        await enableProjectWorkspaceRepository(base);
      } else if (action === "disconnect") {
        await disableProjectWorkspaceRepository(base);
      } else if (action === "enable") {
        await enableGitHubRepositoryConnection({
          installation_id: base.installation_id,
          github_repository_id: base.github_repository_id,
        });
      } else {
        await disableGitHubRepositoryConnection({
          installation_id: base.installation_id,
          github_repository_id: base.github_repository_id,
        });
      }
      await invalidateGitHub();
    } catch (error) {
      setRepositoryError({ repository: fullName, message: githubErrorMessage(error) });
    } finally {
      setRepositoryAction(null);
    }
  };

  return (
    <>
      <header class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>GitHub</h1>
          <p class={ui.pageSubtitle}>
            GitHub App installations, accessible repositories, and workspace access for the selected project.
          </p>
        </div>
        <button
          class={ui.button}
          type="button"
          disabled={installations.isPending || !installations.data?.install_url}
          onClick={install}
        >
          Install GitHub App
        </button>
      </header>

      <Show when={installations.isError}>
        <p class={ui.error} role="alert">{githubErrorMessage(installations.error)}</p>
      </Show>

      <Show when={!installations.isPending} fallback={<p class={ui.muted}>Loading GitHub installations...</p>}>
        <Show
          when={(installations.data?.installations.length ?? 0) > 0}
          fallback={<p class={ui.emptyState}>No GitHub installations are connected.</p>}
        >
          <section class={"mb-6"}>
            <h2 class={cx(ui.h2, "mb-3")}>Installations</h2>
            <div class={ui.tableWrap}>
              <table class={ui.dataTable}>
                <thead>
                  <tr>
                    <th>Account</th>
                    <th>Repository access</th>
                    <th>Status</th>
                    <th>Updated</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  <For each={installations.data?.installations ?? []}>
                    {(installation) => <GitHubInstallationRow installation={installation} />}
                  </For>
                </tbody>
              </table>
            </div>
          </section>

          <section>
            <div class={cx(ui.toolbar, "mb-3")}>
              <div>
                <h2 class={ui.h2}>Repositories</h2>
                <p class={ui.pageSubtitle}>
                  Allow repositories to be mounted by runs in {scope.selectedProject()?.name ?? "the selected project"}.
                </p>
              </div>
            </div>

            <Show when={repositories.isError}>
              <p class={ui.error} role="alert">{githubErrorMessage(repositories.error)}</p>
            </Show>

            <Show when={activeInstallations().length === 0}>
              <p class={ui.emptyState}>No active GitHub installations can provide repositories.</p>
            </Show>

            <Show when={activeInstallations().length > 0 && !repositories.isError}>
              <Show when={!repositories.isPending} fallback={<p class={ui.muted}>Loading GitHub repositories...</p>}>
                <Show
                  when={(repositories.data?.length ?? 0) > 0}
                  fallback={<p class={ui.emptyState}>No repositories are accessible to the connected GitHub App installations.</p>}
                >
                  <div class={ui.tableWrap}>
                    <table class={"min-w-280"}>
                      <thead>
                        <tr>
                          <th>Repository</th>
                          <th>Installation</th>
                          <th>Access</th>
                          <th>Workspace project</th>
                          <th>Default branch</th>
                          <th>Updated</th>
                          <th>Actions</th>
                        </tr>
                      </thead>
                      <tbody>
                        <For each={repositories.data ?? []}>
                          {(repository) => (
                            <RepositoryRow
                              repository={repository}
                              accountLogin={installationAccountByID()[repository.installation_id] ?? "-"}
                              action={repositoryAction()}
                              error={repositoryError()?.repository === repositoryFullName(repository) ? repositoryError()?.message ?? null : null}
                              onAction={updateRepository}
                            />
                          )}
                        </For>
                      </tbody>
                    </table>
                  </div>
                </Show>
              </Show>
            </Show>
          </section>
        </Show>
      </Show>
    </>
  );
}
