import { A } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show, type JSX } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { runHref } from "../features/runs/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { getCurrentDeployment } from "../lib/deployments";
import { cancelRun, listRuns, type Run } from "../lib/runs";
import { useScope } from "../lib/scope";
import { sessionConsolePath } from "../lib/sessions";
import { cancelToken, completeToken, listTokens, type TokenListItem } from "../lib/tokens";
import { ConfirmModal } from "../ui/ConfirmModal";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Modal } from "../ui/Modal";
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
  | { kind: "run"; run: Run };

function actionErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) return error.message;
  return fallback;
}

function attentionRows(tokens: TokenListItem[], waiting: Run[]): AttentionRow[] {
  const byTimeout = [...tokens].sort((a, b) => a.timeout_at.localeCompare(b.timeout_at));
  const byAge = waiting
    .filter((run) => !!run.session_id)
    .sort((a, b) => a.created_at.localeCompare(b.created_at));
  return [
    ...byTimeout.map((token): AttentionRow => ({ kind: "token", token })),
    ...byAge.map((run): AttentionRow => ({ kind: "run", run })),
  ];
}

function Section(props: { title: string; count?: number; viewAll: JSX.Element; children: JSX.Element }) {
  return (
    <section class="min-w-0">
      <SectionHeader title={props.title} count={props.count} actions={props.viewAll} />
      {props.children}
    </section>
  );
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

function CompleteTokenModal(props: {
  token: TokenListItem;
  projectID: string;
  environmentID: string;
  onClose: () => void;
  onCompleted: () => Promise<void>;
}) {
  const [result, setResult] = createSignal("{}");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async (event: Event) => {
    event.preventDefault();
    let parsed: unknown;
    try {
      parsed = JSON.parse(result());
    } catch {
      setError("Result must be valid JSON.");
      return;
    }
    setError(null);
    setSubmitting(true);
    try {
      await completeToken(props.token.id, { projectID: props.projectID, environmentID: props.environmentID }, {
        result: parsed,
        idempotency_key: crypto.randomUUID(),
      });
      await props.onCompleted();
      props.onClose();
    } catch (cause) {
      setError(actionErrorMessage(cause, "Could not complete this Token."));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal title="Complete Token" onClose={props.onClose} closeDisabled={submitting()}>
      <form onSubmit={submit}>
        <p class={ui.modalIntro}>
          The result is delivered to the waiting Run as JSON. Token <IDText value={props.token.id} />.
        </p>
        <label class={ui.field}>
          <span>Result (JSON)</span>
          <textarea
            class={`${ui.textarea} font-mono`}
            rows={6}
            value={result()}
            onInput={(event) => setResult(event.currentTarget.value)}
            spellcheck={false}
            autofocus
          />
        </label>
        <Show when={error()}>
          <p class={ui.fieldError} role="alert">{error()}</p>
        </Show>
        <div class={ui.modalActions}>
          <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={props.onClose}>
            Cancel
          </button>
          <button type="submit" class={ui.button} disabled={submitting()}>
            {submitting() ? "Completing..." : "Complete"}
          </button>
        </div>
      </form>
    </Modal>
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

  const attention = createMemo(() => attentionRows(pendingTokens.data?.tokens ?? [], waitingRuns.data?.runs ?? []));
  const inProgressItems = createMemo(() => inProgress.data?.runs ?? []);
  const failedItems = createMemo(() => failedRuns.data?.runs ?? []);

  const [completing, setCompleting] = createSignal<TokenListItem | null>(null);
  const [cancellingToken, setCancellingToken] = createSignal<TokenListItem | null>(null);
  const [cancellingRun, setCancellingRun] = createSignal<Run | null>(null);

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
              <Section
                title="Needs you"
                count={attention().length}
                viewAll={
                  <>
                    <A class={ui.ghostButton} href="/tokens">Tokens</A>
                    <A class={ui.ghostButton} href="/sessions">Sessions</A>
                  </>
                }
              >
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
                            <td><IDText value={row.token.id} /></td>
                            <td><TagList tags={row.token.tags} /></td>
                            <td><StatusBadge resource="token" status="pending" /></td>
                            <td><RelativeTime value={row.token.timeout_at} prefix="expires" /></td>
                            <td class={ui.actionsCell}>
                              <div class="flex justify-end gap-1.5">
                                <Show when={can("tokens.complete")}>
                                  <button type="button" class={ui.button} onClick={() => setCompleting(row.token)}>Complete</button>
                                </Show>
                                <Show when={can("tokens.cancel")}>
                                  <button type="button" class={ui.dangerOutlineButton} onClick={() => setCancellingToken(row.token)}>Cancel</button>
                                </Show>
                              </div>
                            </td>
                          </tr>
                        ) : (
                          <tr>
                            <td><span class={ui.muted}>Actor</span></td>
                            <td>
                              <A href={runHref(row.run.id, projectID(), environmentID())} class="font-medium text-console-text hover:text-console-accent">
                                {row.run.entrypoint.id}
                              </A>
                            </td>
                            <td><TagList tags={[]} /></td>
                            <td><StatusBadge resource="run" status="waiting" /></td>
                            <td><RelativeTime value={row.run.created_at} /></td>
                            <td class={ui.actionsCell}>
                              <A class={ui.secondaryButton} href={sessionConsolePath(row.run.session_id!, projectID(), environmentID())}>Open Session</A>
                            </td>
                          </tr>
                        )}
                      </For>
                    </DataTable>
                    <MoreRows total={attention().length} />
                  </Show>
                </Show>
              </Section>

              <Section title="Recent failures" count={failedItems().length} viewAll={<A class={ui.ghostButton} href="/runs">Runs</A>}>
                <Show when={!failedRuns.isPending} fallback={<StatePanel loading="Loading..." />}>
                  <Show when={failedRuns.isError}>
                    <StatePanel error={actionErrorMessage(failedRuns.error, "Could not load failed Runs.")} />
                  </Show>
                  <Show
                    when={failedItems().length > 0}
                    fallback={<StatePanel empty="No failed Runs." hint="Application and system failures appear here." />}
                  >
                    <DataTable columns={["Entrypoint", "Kind", "Status", "Ended", "Run"]}>
                      <For each={failedItems().slice(0, SECTION_ROWS)}>
                        {(run) => (
                          <tr>
                            <td>
                              <A href={runHref(run.id, projectID(), environmentID())} class="font-medium text-console-text hover:text-console-accent">{run.entrypoint.id}</A>
                            </td>
                            <td><span class={ui.muted}>{run.entrypoint.kind}</span></td>
                            <td><StatusBadge resource="run" status={run.status} /></td>
                            <td><RelativeTime value={run.terminal_at ?? run.created_at} /></td>
                            <td><IDText value={run.id} /></td>
                          </tr>
                        )}
                      </For>
                    </DataTable>
                    <MoreRows total={failedItems().length} />
                  </Show>
                </Show>
              </Section>

              <Section title="In progress" count={inProgressItems().length} viewAll={<A class={ui.ghostButton} href="/runs">Runs</A>}>
                <Show when={!inProgress.isPending} fallback={<StatePanel loading="Loading..." />}>
                  <Show when={inProgress.isError}>
                    <StatePanel error={actionErrorMessage(inProgress.error, "Could not load Runs in progress.")} />
                  </Show>
                  <Show
                    when={inProgressItems().length > 0}
                    fallback={<StatePanel empty="Nothing is running." hint="Queued, running, retrying, and cancelling Runs appear here." />}
                  >
                    <DataTable columns={["Entrypoint", "Kind", "Status", "Attempt", "Created", { label: "Actions", srOnly: true }]}>
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
                            <td class={ui.actionsCell}>
                              <Show when={can("runs.manage") && run.status !== "cancel_requested"}>
                                <button type="button" class={ui.dangerOutlineButton} onClick={() => setCancellingRun(run)}>Cancel</button>
                              </Show>
                            </td>
                          </tr>
                        )}
                      </For>
                    </DataTable>
                    <MoreRows total={inProgressItems().length} />
                  </Show>
                </Show>
              </Section>

              <Section title="Deployments" viewAll={<A class={ui.ghostButton} href="/deployments">Deployments</A>}>
                <DataTable columns={["Current version", "Digest", "Created", "ID"]}>
                  <tr>
                    <td>
                      <A href={deploymentHref(deployment().id)} class="font-medium text-console-text hover:text-console-accent">
                        {deployment().version}
                      </A>
                    </td>
                    <td><IDText value={deployment().bundle_digest} /></td>
                    <td><RelativeTime value={deployment().created_at} /></td>
                    <td><IDText value={deployment().id} /></td>
                  </tr>
                </DataTable>
              </Section>
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
          <ConfirmModal
            title="Cancel Token"
            confirmLabel="Cancel Token"
            busyLabel="Cancelling..."
            tone="danger"
            onClose={() => setCancellingToken(null)}
            onConfirm={async () => {
              await cancelToken(token().id, resourceScope(), { idempotency_key: crypto.randomUUID() });
              await refreshTokens();
            }}
            errorMessage={(error) => actionErrorMessage(error, "Could not cancel this Token.")}
          >
            The waiting Run receives a cancelled Token and cannot be completed later. Token <IDText value={token().id} />.
          </ConfirmModal>
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
