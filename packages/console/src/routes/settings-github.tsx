import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { ApiError } from "../lib/api";
import {
  connectProjectGitHubRepository,
  disconnectProjectGitHubRepository,
  listGitHubInstallationRepositories,
  listGitHubInstallations,
  type GitHubInstallation,
  type GitHubRepository,
} from "../lib/github";
import { useScope } from "../lib/scope";
import { ActionMenu, type ActionMenuItem } from "../ui/ActionMenu";
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

type RepositoryAction = "connect" | "disconnect";

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

function repositoryConnected(repository: GitHubRepository): boolean {
  return repository.project_github_repository?.connected === true;
}

function repositoryFullName(repository: GitHubRepository): string {
  return repository.full_name ?? repository.name ?? "-";
}

function repositoryScopeText(repository: GitHubRepository): string {
  const connection = repository.project_github_repository;
  if (!connection?.connected) return "Not connected";
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
            <ActionMenu
              label={`Actions for ${props.installation.account_login}`}
              items={[{
                label: "Configure",
                href: url(),
                external: true,
              }]}
            />
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
  const fullName = () => repositoryFullName(props.repository);
  const busy = (action: RepositoryAction) =>
    props.action?.repository === fullName() && props.action.action === action;
  const items = (): ActionMenuItem[] => {
    const actions: ActionMenuItem[] = [];
    if (props.repository.html_url) {
      actions.push({
        label: "Open",
        href: props.repository.html_url,
        external: true,
      });
    }
    if (connected()) {
      actions.push({
        label: "Remove from project",
        busyLabel: busy("disconnect") ? "Removing..." : undefined,
        disabled: busy("disconnect"),
        tone: "danger",
        onSelect: () => props.onAction(props.repository, "disconnect"),
      });
      return actions;
    }
    actions.push({
      label: "Connect to project",
      busyLabel: busy("connect") ? "Connecting..." : undefined,
      disabled: busy("connect"),
      onSelect: () => props.onAction(props.repository, "connect"),
    });
    return actions;
  };
  return (
    <tr class={ui.detailTableRow}>
      <td>
        <div class={ui.tableCellStack}>
          <strong>{fullName()}</strong>
          <div class={ui.muted}>{props.repository.private ? "Private" : "Public"}</div>
        </div>
      </td>
      <td>{props.accountLogin}</td>
      <td>{repositoryScopeText(props.repository)}</td>
      <td><code>{props.repository.default_branch ?? "-"}</code></td>
      <td>{formatDate(props.repository.updated_at)}</td>
      <td class={ui.actionsCell}>
        <ActionMenu label={`Actions for ${fullName()}`} items={items()} />
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
        github_repository_id: repository.github_repository_id,
        project_id: scope.selectedProjectID(),
      };
      if (action === "connect") {
        await connectProjectGitHubRepository(base);
      } else if (action === "disconnect") {
        await disconnectProjectGitHubRepository(base);
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
            GitHub App installations and repositories connected to the selected project.
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
                    <th><span class="sr-only">Actions</span></th>
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
                  Connect repositories that runs can mount in {scope.selectedProject()?.name ?? "the selected project"}.
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
                          <th>Project</th>
                          <th>Default branch</th>
                          <th>Updated</th>
                          <th><span class="sr-only">Actions</span></th>
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
