import { useNavigate, useParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { runHref } from "../features/runs/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { useScope } from "../lib/scope";
import { sessionConsolePath } from "../lib/sessions";
import {
  decodeExecOutput,
  deleteWorkspace,
  execWorkspace,
  getWorkspace,
  getWorkspaceExec,
  isTerminalExecStatus,
  parseExecCommand,
  type WorkspaceExecProcess,
  type WorkspaceScope,
} from "../lib/workspaces";
import { ConfirmModal } from "../ui/ConfirmModal";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { DetailItem, DetailList, Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { SectionHeader } from "../ui/SectionHeader";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

type ExecHistoryEntry = {
  process: WorkspaceExecProcess;
  command: string[];
  startedAt: string;
};

function workspaceErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 404) return "Workspace not found.";
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to view this Workspace.";
  if (error instanceof ApiError) return error.message;
  return "Could not load this Workspace.";
}

function actionErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) return error.message;
  return fallback;
}

function ExecProcessCard(props: { workspaceID: string; scope: WorkspaceScope; entry: ExecHistoryEntry }) {
  const process = createQuery(() => ({
    queryKey: ["workspace-exec", props.workspaceID, props.entry.process.process_id, props.scope.projectID, props.scope.environmentID],
    queryFn: () => getWorkspaceExec(props.workspaceID, props.entry.process.process_id, props.scope),
    initialData: props.entry.process,
    retry: false,
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status && !isTerminalExecStatus(status) ? 2_000 : false;
    },
  }));
  const current = () => process.data ?? props.entry.process;
  const stdout = createMemo(() => decodeExecOutput(current().stdout_base64));
  const stderr = createMemo(() => decodeExecOutput(current().stderr_base64));

  return (
    <section class="border border-console-border bg-console-surface p-4">
      <div class="mb-3 flex flex-wrap items-center gap-x-4 gap-y-1.5">
        <StatusBadge resource="workspace_exec" status={current().status} />
        <code class="font-mono text-[11.5px] text-console-text">{props.entry.command.join(" ")}</code>
        <span class={ui.muted}>
          exit code{" "}
          <Show when={current().exit_code !== undefined} fallback={<span class="text-console-faint">—</span>}>
            <strong class="font-medium text-console-text">{current().exit_code}</strong>
          </Show>
        </span>
        <span class={ui.muted}>started <RelativeTime value={props.entry.startedAt} /></span>
        <IDText value={current().process_id} />
      </div>
      <Show when={process.isError}>
        <StatePanel error={actionErrorMessage(process.error, "Could not poll this process.")} />
      </Show>
      <Show when={current().error}>
        {(failure) => <p class={ui.error} role="alert">Terminal reason: <code>{failure().terminal_reason_code}</code></p>}
      </Show>
      <Show when={!isTerminalExecStatus(current().status)}>
        <p class={ui.muted}>Waiting for the process to finish...</p>
      </Show>
      <Show when={isTerminalExecStatus(current().status)}>
        <div class="grid gap-3">
          <div>
            <h3 class={`${ui.h3} mb-1.5`}>stdout</h3>
            <pre class={ui.codeBlock}>{stdout() || " "}</pre>
          </div>
          <Show when={stderr() !== ""}>
            <div>
              <h3 class={`${ui.h3} mb-1.5`}>stderr</h3>
              <pre class={ui.codeBlock}>{stderr()}</pre>
            </div>
          </Show>
        </div>
      </Show>
    </section>
  );
}

function ExecPanel(props: { workspaceID: string; scope: WorkspaceScope }) {
  const [commandLine, setCommandLine] = createSignal("");
  const [cwd, setCwd] = createSignal("");
  const [timeoutText, setTimeoutText] = createSignal("");
  const [confirming, setConfirming] = createSignal(false);
  const [history, setHistory] = createSignal<ExecHistoryEntry[]>([]);
  const argv = createMemo(() => parseExecCommand(commandLine()));

  const submit = async () => {
    const command = argv();
    const nextCwd = cwd().trim();
    const nextTimeout = timeoutText().trim();
    const process = await execWorkspace(props.workspaceID, props.scope, {
      command,
      ...(nextCwd ? { cwd: nextCwd } : {}),
      ...(nextTimeout ? { timeout: nextTimeout } : {}),
      idempotency_key: crypto.randomUUID(),
    });
    setHistory((entries) => [{ process, command, startedAt: new Date().toISOString() }, ...entries].slice(0, 20));
  };

  return (
    <>
      <Panel title="Exec">
        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (argv().length > 0) setConfirming(true);
          }}
        >
          <label class={ui.field}>
            <span>Command (argv, split on whitespace)</span>
            <input
              type="text"
              class={`${ui.input} font-mono`}
              value={commandLine()}
              onInput={(event) => setCommandLine(event.currentTarget.value)}
              placeholder="ls -la /work"
              autocomplete="off"
              spellcheck={false}
            />
          </label>
          <div class="grid grid-cols-2 gap-3 max-sm:grid-cols-1">
            <label class={ui.field}>
              <span>Working directory (optional)</span>
              <input
                type="text"
                class={`${ui.input} font-mono`}
                value={cwd()}
                onInput={(event) => setCwd(event.currentTarget.value)}
                placeholder="/work"
                autocomplete="off"
                spellcheck={false}
              />
            </label>
            <label class={ui.field}>
              <span>Timeout (optional, e.g. 30s)</span>
              <input
                type="text"
                class={`${ui.input} font-mono`}
                value={timeoutText()}
                onInput={(event) => setTimeoutText(event.currentTarget.value)}
                placeholder="30s"
                autocomplete="off"
                spellcheck={false}
              />
            </label>
          </div>
          <div class={ui.actionRow}>
            <button type="submit" class={ui.button} disabled={argv().length === 0}>
              Run command
            </button>
          </div>
        </form>
      </Panel>

      <Show when={history().length > 0}>
        <section class="min-w-0">
          <SectionHeader title="Processes" count={history().length} subtitle="Started from this page; the list clears when you leave it." />
          <div class="grid gap-3">
            <For each={history()}>
              {(entry) => <ExecProcessCard workspaceID={props.workspaceID} scope={props.scope} entry={entry} />}
            </For>
          </div>
        </section>
      </Show>

      <Show when={confirming()}>
        <ConfirmModal
          title="Run command in Workspace"
          confirmLabel="Run"
          busyLabel="Starting..."
          onClose={() => setConfirming(false)}
          onConfirm={submit}
          errorMessage={(error) => actionErrorMessage(error, "Could not start this process.")}
        >
          The command runs inside the Workspace and can change its files. <code class="font-mono text-console-text">{argv().join(" ")}</code>
        </ConfirmModal>
      </Show>
    </>
  );
}

export function WorkspaceDetail() {
  const params = useParams();
  const scope = useScope();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const workspaceID = createMemo(() => params["workspace_id"]?.trim() ?? "");
  const projectID = createMemo(() => scope.selectedProjectID());
  const environmentID = createMemo(() => scope.selectedEnvironmentID());
  const hasScope = createMemo(() => projectID() !== "" && environmentID() !== "");
  const resourceScope = (): WorkspaceScope => ({ projectID: projectID(), environmentID: environmentID() });
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const workspace = createQuery(() => ({
    queryKey: ["workspace", workspaceID(), projectID(), environmentID()],
    queryFn: () => getWorkspace(workspaceID(), resourceScope()),
    enabled: workspaceID() !== "" && hasScope(),
    retry: false,
  }));
  const [deleting, setDeleting] = createSignal(false);

  return (
    <section class={ui.page}>
      <PageHeader
        title="Workspace"
        back={{ href: "/workspaces", label: "Workspaces" }}
        badge={<Show when={workspace.data}>{(current) => <StatusBadge resource="workspace" status={current().status} />}</Show>}
        subtitle={<Show when={workspace.data}>{(current) => <IDText value={current().id} mode="full" />}</Show>}
        actions={
          <Show when={workspace.data && hasPermission(me.data, "workspaces.delete")}>
            <button type="button" class={ui.dangerOutlineButton} onClick={() => setDeleting(true)}>Delete</button>
          </Show>
        }
      />

      <Show when={workspaceID() !== ""} fallback={<StatePanel error="Workspace ID is required." />}>
        <Show when={hasScope()} fallback={<StatePanel empty="Select a project and environment." />}>
          <Show when={!workspace.isPending} fallback={<StatePanel loading="Loading Workspace..." />}>
            <Show
              when={workspace.data}
              fallback={
                <StatePanel empty={workspaceErrorMessage(workspace.error)}>
                  <button class={ui.secondaryButton} type="button" onClick={() => void workspace.refetch()}>
                    Retry
                  </button>
                </StatePanel>
              }
            >
              {(current) => (
                <div class="grid grid-cols-[minmax(0,1fr)_310px] items-start gap-3.5 max-[960px]:grid-cols-1">
                  <div class="grid gap-3.5">
                    <Panel title="Secret placements">
                      <Show when={current().secrets.length > 0} fallback={<StatePanel empty="No Secrets are attached." />}>
                        <DataTable columns={["Secret", "Placement", "Target"]} minWidth="min-w-0">
                          <For each={current().secrets}>
                            {(secret) => (
                              <tr>
                                <td><code>{secret.name}</code></td>
                                <td><span class={ui.muted}>{secret.env !== undefined ? "Env var" : "File"}</span></td>
                                <td><code>{secret.env ?? secret.file}</code></td>
                              </tr>
                            )}
                          </For>
                        </DataTable>
                      </Show>
                    </Panel>

                    <Show when={hasPermission(me.data, "workspace.exec.create")}>
                      <ExecPanel workspaceID={current().id} scope={resourceScope()} />
                    </Show>
                  </div>

                  <DetailList title="Workspace details">
                    <DetailItem label="State"><StatusBadge resource="workspace" status={current().status} /></DetailItem>
                    <DetailItem label="Key">
                      <Show when={current().key} fallback={<span class="text-console-faint">—</span>}>{(key) => <code>{key()}</code>}</Show>
                    </DetailItem>
                    <DetailItem label="Sandbox"><code>{current().sandbox_id}</code></DetailItem>
                    <DetailItem label="Deployment">
                      <IDText value={current().deployment_id} mode="link" href={deploymentHref(current().deployment_id)} />
                    </DetailItem>
                    <DetailItem label="Owner">
                      <Show when={current().owner?.session_id}>
                        {(sessionID) => <IDText value={sessionID()} mode="link" href={sessionConsolePath(sessionID(), projectID(), environmentID())} />}
                      </Show>
                      <Show when={!current().owner?.session_id && current().owner?.run_id}>
                        {(runID) => <IDText value={runID()} mode="link" href={runHref(runID(), projectID(), environmentID())} />}
                      </Show>
                      <Show when={!current().owner?.session_id && !current().owner?.run_id}>
                        <span class="text-console-faint">—</span>
                      </Show>
                    </DetailItem>
                    <DetailItem label="Last activity"><RelativeTime value={current().last_activity_at} /></DetailItem>
                    <DetailItem label="Created"><RelativeTime value={current().created_at} /></DetailItem>
                    <DetailItem label="Updated"><RelativeTime value={current().updated_at} /></DetailItem>
                  </DetailList>
                </div>
              )}
            </Show>
          </Show>
        </Show>
      </Show>

      <Show when={deleting() && workspace.data}>
        {(current) => (
          <ConfirmModal
            title="Delete Workspace"
            confirmLabel="Delete Workspace"
            busyLabel="Deleting..."
            tone="danger"
            onClose={() => setDeleting(false)}
            onConfirm={async () => {
              await deleteWorkspace(current().id, resourceScope(), { idempotency_key: crypto.randomUUID() });
              await queryClient.invalidateQueries({ queryKey: ["workspaces"] });
              navigate("/workspaces");
            }}
            errorMessage={(error) => actionErrorMessage(error, "Could not delete this Workspace.")}
          >
            The Workspace filesystem is removed and cannot be recovered. A Workspace owned by an open Session or an active Run cannot be deleted. Workspace <IDText value={current().id} />.
          </ConfirmModal>
        )}
      </Show>
    </section>
  );
}
