import { A } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { runHref } from "../features/runs/navigation";
import { CancelTokenModal, CompleteTokenModal } from "../features/tokens/TokenActions";
import { tokenHref } from "../features/tokens/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { getCurrentDeployment } from "../lib/deployments";
import { cancelRun, listRuns, type RunListItem } from "../lib/runs";
import { useScope } from "../lib/scope";
import { listSessions, sessionConsolePath, type Session } from "../lib/sessions";
import { listTokens, type TokenListItem } from "../lib/tokens";
import { ActionMenu, type ActionMenuItem } from "../ui/ActionMenu";
import { ConfirmModal } from "../ui/ConfirmModal";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { SectionHeader } from "../ui/SectionHeader";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";
import { TagList } from "../ui/TagList";

const SECTION_ROWS = 5;

type AttentionRow =
  | { kind: "token"; token: TokenListItem }
  | { kind: "run"; run: RunListItem };

type FailureRow =
  | { kind: "run"; run: RunListItem; at: string }
  | { kind: "session"; session: Session; at: string };

function actionErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) return error.message;
  return fallback;
}

function attentionRows(tokens: TokenListItem[], waiting: RunListItem[]): AttentionRow[] {
  const byTimeout = [...tokens].sort((a, b) => a.timeout_at.localeCompare(b.timeout_at));
  const byAge = waiting
    .filter((run) => !!run.session_id)
    .sort((a, b) => a.created_at.localeCompare(b.created_at));
  return [
    ...byTimeout.map((token): AttentionRow => ({ kind: "token", token })),
    ...byAge.map((run): AttentionRow => ({ kind: "run", run })),
  ];
}

function failureRows(runs: RunListItem[], sessions: Session[]): FailureRow[] {
  const rows: FailureRow[] = [
    ...runs.map((run): FailureRow => ({ kind: "run", run, at: run.terminal_at ?? run.created_at })),
    ...sessions.map((session): FailureRow => ({ kind: "session", session, at: session.updated_at })),
  ];
  return rows.sort((a, b) => Date.parse(b.at) - Date.parse(a.at));
}

function MoreRows(props: { total: number }) {
  return (
    <Show when={props.total > SECTION_ROWS}>
      <p class="mt-1.5 font-mono text-[11px] text-console-subtle">and {props.total - SECTION_ROWS} more</p>
    </Show>
  );
}

function CliOnboarding() {
  return (
    <div class={"border border-console-border-strong bg-console-surface"}>
      <div class={"border-b border-console-border bg-console-bg-panel px-4 py-3"}>
        <h2 class={ui.h2}>Set up your first task</h2>
        <p class={ui.pageSubtitle}>
          Create a task project, add a task file, then deploy it to the selected project and environment.
        </p>
      </div>
      <div class={"grid gap-0 divide-y divide-console-border-soft"}>
        <section class={"grid gap-2 px-4 py-3"}>
          <div class={"flex items-center gap-2"}>
            <span class={"grid size-5 shrink-0 place-items-center border border-console-border bg-console-bg-panel font-mono text-[10.5px] font-medium text-console-muted"}>1</span>
            <h3 class={"m-0 text-[13px] font-medium text-console-text"}>Initialize a task project</h3>
          </div>
          <code class={"block overflow-x-auto border border-console-border bg-console-bg-panel px-3 py-2 font-mono text-[12px] text-console-text"}>
            helmr init --dir ./my-helmr-tasks
          </code>
        </section>
        <section class={"grid gap-2 px-4 py-3"}>
          <div class={"flex items-center gap-2"}>
            <span class={"grid size-5 shrink-0 place-items-center border border-console-border bg-console-bg-panel font-mono text-[10.5px] font-medium text-console-muted"}>2</span>
            <h3 class={"m-0 text-[13px] font-medium text-console-text"}>Define tasks under your configured directory</h3>
          </div>
          <code class={"block overflow-x-auto whitespace-pre border border-console-border bg-console-bg-panel px-3 py-2 font-mono text-[12px] leading-relaxed text-console-text"}>{`import { defineConfig } from "@helmr/sdk"

export default defineConfig({
  dirs: ["tasks"],
})`}</code>
        </section>
        <section class={"grid gap-2 px-4 py-3"}>
          <div class={"flex items-center gap-2"}>
            <span class={"grid size-5 shrink-0 place-items-center border border-console-border bg-console-bg-panel font-mono text-[10.5px] font-medium text-console-muted"}>3</span>
            <h3 class={"m-0 text-[13px] font-medium text-console-text"}>Deploy task definitions</h3>
          </div>
          <code class={"block overflow-x-auto border border-console-border bg-console-bg-panel px-3 py-2 font-mono text-[12px] text-console-text"}>
            helmr deploy ./my-helmr-tasks --project PROJECT --env ENVIRONMENT
          </code>
        </section>
      </div>
    </div>
  );
}

export function Overview() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const enabled = () => !!projectID() && !!environmentID();
  const resourceScope = () => ({ projectID: projectID(), environmentID: environmentID() });
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const can = (permission: string) => hasPermission(me.data, permission);

  const current = createQuery(() => ({
    queryKey: ["deployments", "current", projectID(), environmentID()],
    queryFn: () => getCurrentDeployment(resourceScope()),
    enabled: enabled(),
    retry: false,
  }));
  const hasDeployment = () => !!current.data;

  const pendingTokens = createQuery(() => ({
    queryKey: ["tokens", "pending", projectID(), environmentID()],
    queryFn: () => listTokens(resourceScope(), { status: "pending", limit: 100 }),
    enabled: enabled() && hasDeployment(),
    retry: false,
    refetchInterval: 5_000,
  }));
  const waitingRuns = createQuery(() => ({
    queryKey: ["runs", "overview", "waiting", projectID(), environmentID()],
    queryFn: () => listRuns({ ...resourceScope(), statuses: ["waiting"], limit: 100 }),
    enabled: enabled() && hasDeployment(),
    retry: false,
    refetchInterval: 5_000,
  }));
  const inProgress = createQuery(() => ({
    queryKey: ["runs", "overview", "in-progress", projectID(), environmentID()],
    queryFn: () => listRuns({
      ...resourceScope(),
      statuses: ["queued", "running", "retry_delayed", "cancel_requested"],
      limit: 100,
    }),
    enabled: enabled() && hasDeployment(),
    retry: false,
    refetchInterval: 5_000,
  }));
  const failedRuns = createQuery(() => ({
    queryKey: ["runs", "overview", "failed", projectID(), environmentID()],
    queryFn: () => listRuns({ ...resourceScope(), statuses: ["failed", "system_failed"], limit: 100 }),
    enabled: enabled() && hasDeployment(),
    retry: false,
    refetchInterval: 5_000,
  }));
  const failedSessions = createQuery(() => ({
    queryKey: ["sessions", "overview", "failed", projectID(), environmentID()],
    queryFn: () => listSessions({ ...resourceScope(), statuses: ["failed"], limit: 100 }),
    enabled: enabled() && hasDeployment(),
    retry: false,
    refetchInterval: 5_000,
  }));

  const attention = createMemo(() => attentionRows(pendingTokens.data?.tokens ?? [], waitingRuns.data?.runs ?? []));
  const inProgressItems = createMemo(() => inProgress.data?.runs ?? []);
  const failedItems = createMemo(() => failureRows(failedRuns.data?.runs ?? [], failedSessions.data?.sessions ?? []));

  const [completing, setCompleting] = createSignal<TokenListItem | null>(null);
  const [cancellingToken, setCancellingToken] = createSignal<TokenListItem | null>(null);
  const [cancellingRun, setCancellingRun] = createSignal<RunListItem | null>(null);

  const tokenActions = (token: TokenListItem): ActionMenuItem[] => [
    ...(can("tokens.complete") ? [{ label: "Complete…", onSelect: () => setCompleting(token) }] : []),
    ...(can("tokens.cancel") ? [{ label: "Cancel…", tone: "danger" as const, onSelect: () => setCancellingToken(token) }] : []),
  ];

  const refreshTokens = async () => {
    await queryClient.invalidateQueries({ queryKey: ["tokens"] });
  };
  const refreshRuns = async () => {
    await queryClient.invalidateQueries({ queryKey: ["runs"] });
  };

  return (
    <section class={ui.page}>
      <PageHeader
        title="Overview"
        subtitle="What needs a person in this environment, what failed, what is running, and what is deployed."
      />

      <Show when={current.isError}>
        <StatePanel error={actionErrorMessage(current.error, "Could not load the current Deployment.")} />
      </Show>
      <Show when={current.isPending && enabled()}>
        <StatePanel loading="Loading overview..." />
      </Show>

      <Show when={current.isSuccess}>
        <Show when={current.data} fallback={<CliOnboarding />}>
          {(deployment) => (
            <div class="grid gap-6">
              <section class="min-w-0">
                <SectionHeader
                  title="Needs you"
                  count={attention().length}
                  actions={
                    <>
                      <A class={ui.ghostButton} href="/tokens">Tokens</A>
                      <A class={ui.ghostButton} href="/sessions">Sessions</A>
                    </>
                  }
                />
                <Show when={!pendingTokens.isPending && !waitingRuns.isPending} fallback={<StatePanel loading="Loading..." />}>
                  <Show when={pendingTokens.isError || waitingRuns.isError}>
                    <StatePanel error={actionErrorMessage(pendingTokens.error ?? waitingRuns.error, "Could not load pending work.")} />
                  </Show>
                  <Show
                    when={attention().length > 0}
                    fallback={<StatePanel empty="Nothing is waiting on you." hint="Pending Tokens and waiting Actor Runs appear here." />}
                  >
                    <DataTable columns={["Kind", "Item", "Tags", "State", "Time", { label: "Actions", srOnly: true }]} minWidth="min-w-180">
                      <For each={attention().slice(0, SECTION_ROWS)}>
                        {(row) => row.kind === "token" ? (
                          <tr>
                            <td><span class={ui.muted}>Token</span></td>
                            <td><IDText value={row.token.id} mode="link" href={tokenHref(row.token.id)} /></td>
                            <td><TagList tags={row.token.tags} /></td>
                            <td><StatusBadge resource="token" status="pending" /></td>
                            <td><span class={ui.muted}>expires </span><RelativeTime value={row.token.timeout_at} /></td>
                            <td class={ui.actionsCell}>
                              <Show when={tokenActions(row.token).length > 0}>
                                <ActionMenu label={`Actions for Token ${row.token.id}`} items={tokenActions(row.token)} />
                              </Show>
                            </td>
                          </tr>
                        ) : (
                          <tr>
                            <td><span class={ui.muted}>Actor</span></td>
                            <td>
                              <A href={sessionConsolePath(row.run.session_id!, projectID(), environmentID())} class="font-medium text-console-text hover:text-console-accent">
                                {row.run.entrypoint.id}
                              </A>
                            </td>
                            <td><TagList tags={[]} /></td>
                            <td><StatusBadge resource="run" status="waiting" /></td>
                            <td><RelativeTime value={row.run.created_at} /></td>
                            <td class={ui.actionsCell}>
                              <ActionMenu
                                label={`Actions for ${row.run.entrypoint.id}`}
                                items={[{ label: "Open Session", href: sessionConsolePath(row.run.session_id!, projectID(), environmentID()) }]}
                              />
                            </td>
                          </tr>
                        )}
                      </For>
                    </DataTable>
                    <MoreRows total={attention().length} />
                  </Show>
                </Show>
              </section>

              <section class="min-w-0">
                <SectionHeader
                  title="Recent failures"
                  count={failedItems().length}
                  actions={
                    <>
                      <A class={ui.ghostButton} href="/runs">Runs</A>
                      <A class={ui.ghostButton} href="/sessions">Sessions</A>
                    </>
                  }
                />
                <Show when={!failedRuns.isPending && !failedSessions.isPending} fallback={<StatePanel loading="Loading..." />}>
                  <Show when={failedRuns.isError || failedSessions.isError}>
                    <StatePanel error={actionErrorMessage(failedRuns.error ?? failedSessions.error, "Could not load recent failures.")} />
                  </Show>
                  <Show
                    when={failedItems().length > 0}
                    fallback={<StatePanel empty="No recent failures." hint="Failed Runs and failed Actor Sessions appear here with the Run that caused them." />}
                  >
                    <DataTable columns={["Entrypoint", "Kind", "Status", "Ended", "Run"]}>
                      <For each={failedItems().slice(0, SECTION_ROWS)}>
                        {(row) => row.kind === "run" ? (
                          <tr>
                            <td>
                              <A href={runHref(row.run.id, projectID(), environmentID())} class="font-medium text-console-text hover:text-console-accent">{row.run.entrypoint.id}</A>
                            </td>
                            <td><span class={ui.muted}>{row.run.entrypoint.kind}</span></td>
                            <td><StatusBadge resource="run" status={row.run.status} /></td>
                            <td><RelativeTime value={row.at} /></td>
                            <td><IDText value={row.run.id} mode="link" href={runHref(row.run.id, projectID(), environmentID())} /></td>
                          </tr>
                        ) : (
                          <tr>
                            <td>
                              <A href={sessionConsolePath(row.session.id, projectID(), environmentID())} class="font-medium text-console-text hover:text-console-accent">{row.session.actor_id}</A>
                            </td>
                            <td><span class={ui.muted}>actor session</span></td>
                            <td><StatusBadge resource="session" status={row.session.status} /></td>
                            <td><RelativeTime value={row.at} /></td>
                            <td>
                              <IDText
                                value={row.session.failure?.details.run_id ?? ""}
                                mode="link"
                                href={row.session.failure?.details.run_id ? runHref(row.session.failure.details.run_id, projectID(), environmentID()) : undefined}
                              />
                            </td>
                          </tr>
                        )}
                      </For>
                    </DataTable>
                    <MoreRows total={failedItems().length} />
                  </Show>
                </Show>
              </section>

              <section class="min-w-0">
                <SectionHeader title="In progress" count={inProgressItems().length} actions={<A class={ui.ghostButton} href="/runs">Runs</A>} />
                <Show when={!inProgress.isPending} fallback={<StatePanel loading="Loading..." />}>
                  <Show when={inProgress.isError}>
                    <StatePanel error={actionErrorMessage(inProgress.error, "Could not load Runs in progress.")} />
                  </Show>
                  <Show
                    when={inProgressItems().length > 0}
                    fallback={<StatePanel empty="Nothing is running." hint="Queued, running, retrying, and cancelling Runs appear here." />}
                  >
                    <DataTable columns={["Entrypoint", "Kind", "Status", "Attempt", "Created", "Run", { label: "Actions", srOnly: true }]}>
                      <For each={inProgressItems().slice(0, SECTION_ROWS)}>
                        {(run) => (
                          <tr>
                            <td>
                              <A href={runHref(run.id, projectID(), environmentID())} class="font-medium text-console-text hover:text-console-accent">{run.entrypoint.id}</A>
                            </td>
                            <td><span class={ui.muted}>{run.entrypoint.kind}</span></td>
                            <td><StatusBadge resource="run" status={run.status} /></td>
                            <td>{run.current_attempt_number}</td>
                            <td><RelativeTime value={run.created_at} /></td>
                            <td><IDText value={run.id} mode="link" href={runHref(run.id, projectID(), environmentID())} /></td>
                            <td class={ui.actionsCell}>
                              <Show when={can("runs.manage") && run.status !== "cancel_requested"}>
                                <ActionMenu
                                  label={`Actions for ${run.entrypoint.id}`}
                                  items={[{ label: "Cancel Run…", tone: "danger", onSelect: () => setCancellingRun(run) }]}
                                />
                              </Show>
                            </td>
                          </tr>
                        )}
                      </For>
                    </DataTable>
                    <MoreRows total={inProgressItems().length} />
                  </Show>
                </Show>
              </section>

              <section class="min-w-0">
                <SectionHeader title="Deployments" actions={<A class={ui.ghostButton} href="/deployments">Deployments</A>} />
                <DataTable columns={["Current version", "Digest", "Created"]}>
                  <tr>
                    <td>
                      <A href={deploymentHref(deployment().id)} class="font-medium text-console-text hover:text-console-accent">
                        {deployment().version}
                      </A>
                    </td>
                    <td><IDText value={deployment().bundle_digest} /></td>
                    <td><RelativeTime value={deployment().created_at} /></td>
                  </tr>
                </DataTable>
              </section>
            </div>
          )}
        </Show>
      </Show>

      <Show when={completing()}>
        {(token) => (
          <CompleteTokenModal
            token={token()}
            projectID={projectID()}
            environmentID={environmentID()}
            onClose={() => setCompleting(null)}
            onCompleted={refreshTokens}
          />
        )}
      </Show>
      <Show when={cancellingToken()}>
        {(token) => (
          <CancelTokenModal
            token={token()}
            projectID={projectID()}
            environmentID={environmentID()}
            onClose={() => setCancellingToken(null)}
            onCancelled={refreshTokens}
          />
        )}
      </Show>
      <Show when={cancellingRun()}>
        {(run) => (
          <ConfirmModal
            title="Cancel Run"
            confirmLabel="Cancel Run"
            busyLabel="Cancelling..."
            tone="danger"
            onClose={() => setCancellingRun(null)}
            onConfirm={async () => {
              await cancelRun(run().id, projectID(), environmentID());
              await refreshRuns();
            }}
            errorMessage={(error) => actionErrorMessage(error, "Could not cancel this Run.")}
          >
            Cancellation is requested for <strong>{run().entrypoint.id}</strong> (<IDText value={run().id} />). A running attempt stops at its next checkpoint.
          </ConfirmModal>
        )}
      </Show>
    </section>
  );
}
