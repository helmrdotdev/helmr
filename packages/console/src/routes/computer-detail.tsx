import { useNavigate, useParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { runHref } from "../features/runs/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { useScope } from "../lib/scope";
import { sessionConsolePath } from "../lib/sessions";
import {
  decodeCommandLogPage,
  deleteComputer,
  execComputer,
  getComputer,
  getCommand,
  listCommandLogs,
  isTerminalCommandStatus,
  listComputerMembers,
  parseExecCommand,
  type CommandReceipt,
  type ComputerScope,
} from "../lib/computers";
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
  receipt: CommandReceipt;
  command: string[];
  startedAt: string;
};

function computerErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 404) return "Computer not found.";
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to view this Computer.";
  if (error instanceof ApiError) return error.message;
  return "Could not load this Computer.";
}

function actionErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to do this.";
  if (error instanceof ApiError) return error.message;
  return fallback;
}

function ComputerMembersPanel(props: { computerID: string; scope: ComputerScope }) {
  const members = createInfiniteQuery(() => ({
    queryKey: ["computer-members", props.computerID, props.scope.projectID, props.scope.environmentID],
    queryFn: ({ pageParam }) => listComputerMembers(props.computerID, props.scope, { cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    refetchInterval: 5_000,
    retry: false,
  }));
  const items = createMemo(() => members.data?.pages.flatMap((page) => page.members) ?? []);
  return (
    <Panel title="Members">
      <Show when={members.isError}>
        <StatePanel error={actionErrorMessage(members.error, "Could not load Computer members.")} />
      </Show>
      <Show when={members.isPending}><StatePanel loading="Loading members..." /></Show>
      <Show when={members.data}>
        <Show when={items().length > 0} fallback={<StatePanel empty="No active members." />}>
          <DataTable columns={["Kind", "ID", "Status"]} minWidth="min-w-0">
            <For each={items()}>{(member) => (
              <tr>
                <td>{member.kind}</td>
                <td>
                  <Show when={member.kind !== "command"} fallback={<IDText value={member.id} />}>
                    <IDText value={member.id} mode="link" href={member.kind === "session"
                      ? sessionConsolePath(member.id, props.scope.projectID, props.scope.environmentID)
                      : runHref(member.id, props.scope.projectID, props.scope.environmentID)} />
                  </Show>
                </td>
                <td>{member.state}</td>
              </tr>
            )}</For>
          </DataTable>
        </Show>
        <Show when={members.hasNextPage}>
          <button class={ui.secondaryButton} disabled={members.isFetchingNextPage} onClick={() => void members.fetchNextPage()}>
            {members.isFetchingNextPage ? "Loading..." : "Load more"}
          </button>
        </Show>
      </Show>
    </Panel>
  );
}

function CommandCard(props: { scope: ComputerScope; entry: ExecHistoryEntry }) {
  const id = () => props.entry.receipt.command_id;
  const command = createQuery(() => ({
    queryKey: ["command", id(), props.scope.projectID, props.scope.environmentID],
    queryFn: ({ signal }) => getCommand(id(), props.scope, signal),
    retry: false,
    refetchInterval: (query) => query.state.data && !isTerminalCommandStatus(query.state.data.status) ? 2_000 : false,
  }));
  const [pages, setPages] = createSignal<{ cursor?: string; stdout: Uint8Array; stderr: Uint8Array }[]>([{ stdout: new Uint8Array(), stderr: new Uint8Array() }]);
  const currentPage = () => pages().at(-1)!;
  const cursor = () => currentPage().cursor;
  const logs = createQuery(() => ({
    queryKey: ["command-logs", id(), props.scope.projectID, props.scope.environmentID, cursor()],
    queryFn: ({ signal }) => listCommandLogs(id(), props.scope, cursor(), signal),
    retry: false,
    refetchInterval: (query) => query.state.data?.output_state === "open" ||
      (query.state.error instanceof ApiError && query.state.error.code === "telemetry_lagging") ? 2_000 : false,
  }));
  const outcome = () => command.data?.outcome;
  const stdout = createMemo(() => decodeCommandLogPage(logs.data?.logs ?? [], "stdout", currentPage().stdout, logs.data?.logs.length === 0 && logs.data.output_state !== "open"));
  const stderr = createMemo(() => decodeCommandLogPage(logs.data?.logs ?? [], "stderr", currentPage().stderr, logs.data?.logs.length === 0 && logs.data.output_state !== "open"));
  const next = () => logs.data?.logs.length ? logs.data.next_cursor : undefined;
  return (
    <section class="border border-console-border bg-console-surface p-4">
      <div class="mb-3 flex flex-wrap items-center gap-x-4 gap-y-1.5">
        <Show when={command.data}>{(current) => <StatusBadge resource="computer_command" status={current().status} />}</Show>
        <code class="font-mono text-[11.5px] text-console-text">{props.entry.command.join(" ")}</code>
        <Show when={outcome()?.exit_code !== undefined}>
          <span class={ui.muted}>exit code <strong class="font-medium text-console-text">{outcome()?.exit_code}</strong></span>
        </Show>
        <span class={ui.muted}>submitted <RelativeTime value={props.entry.startedAt} /></span>
        <IDText value={id()} />
      </div>
      <Show when={command.isError}><StatePanel error={actionErrorMessage(command.error, "Could not load this Command.")} /><button class={ui.secondaryButton} disabled={command.isFetching} onClick={() => void command.refetch()}>Retry Command status</button></Show>
      <Show when={outcome()?.failure}>{(failure) => <p class={ui.error} role="alert">{failure().reason}</p>}</Show>
      <Show when={logs.error instanceof ApiError && logs.error.code === "telemetry_lagging"}>
        <p class={ui.muted}>Waiting for log delivery...</p>
      </Show>
      <Show when={logs.isError && !(logs.error instanceof ApiError && logs.error.code === "telemetry_lagging")}>
        <StatePanel error={actionErrorMessage(logs.error, "Could not load Command output.")} /><button class={ui.secondaryButton} disabled={logs.isFetching} onClick={() => void logs.refetch()}>Retry output</button>
      </Show>
      <Show when={logs.data?.output_state === "unavailable"}>
        <p class={ui.error} role="alert">The Command ended without confirming all output. Available output is shown below.</p>
      </Show>
      <div class="grid gap-3">
        <div><h3 class={`${ui.h3} mb-1.5`}>stdout</h3><pre class={ui.codeBlock}>{stdout().text || " "}</pre></div>
        <Show when={stderr().text !== ""}><div><h3 class={`${ui.h3} mb-1.5`}>stderr</h3><pre class={ui.codeBlock}>{stderr().text}</pre></div></Show>
      </div>
      <div class={ui.actionRow}>
        <button class={ui.secondaryButton} disabled={pages().length === 1} onClick={() => setPages((values) => values.slice(0, -1))}>Previous output</button>
        <button class={ui.secondaryButton} disabled={!next() || logs.isFetching} onClick={() => { const value = next(); if (value) setPages((values) => [...values, { cursor: value, stdout: stdout().pending, stderr: stderr().pending }]); }}>Next output</button>
      </div>
    </section>
  );
}

function ExecPanel(props: { computerID: string; scope: ComputerScope }) {
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
    const receipt = await execComputer(props.computerID, props.scope, {
      command,
      ...(nextCwd ? { cwd: nextCwd } : {}),
      ...(nextTimeout ? { timeout: nextTimeout } : {}),
      idempotency_key: crypto.randomUUID(),
    });
    setHistory((entries) => [{ receipt, command, startedAt: new Date().toISOString() }, ...entries].slice(0, 20));
  };

  return (
    <>
      <Panel title="Run command">
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
          <SectionHeader title="Commands" count={history().length} subtitle="Started from this page; the list clears when you leave it." />
          <div class="grid gap-3">
            <For each={history()}>
              {(entry) => <CommandCard scope={props.scope} entry={entry} />}
            </For>
          </div>
        </section>
      </Show>

      <Show when={confirming()}>
        <ConfirmModal
          title="Run command in Computer"
          confirmLabel="Run"
          busyLabel="Starting..."
          onClose={() => setConfirming(false)}
          onConfirm={submit}
          errorMessage={(error) => actionErrorMessage(error, "Could not start this Command.")}
        >
          The command runs inside the Computer and can change its files. <code class="font-mono text-console-text">{argv().join(" ")}</code>
        </ConfirmModal>
      </Show>
    </>
  );
}

export function ComputerDetail() {
  const params = useParams();
  const scope = useScope();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const computerID = createMemo(() => params["computer_id"]?.trim() ?? "");
  const projectID = createMemo(() => scope.selectedProjectID());
  const environmentID = createMemo(() => scope.selectedEnvironmentID());
  const hasScope = createMemo(() => projectID() !== "" && environmentID() !== "");
  const resourceScope = (): ComputerScope => ({ projectID: projectID(), environmentID: environmentID() });
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const computer = createQuery(() => ({
    queryKey: ["computer", computerID(), projectID(), environmentID()],
    queryFn: () => getComputer(computerID(), resourceScope()),
    enabled: computerID() !== "" && hasScope(),
    retry: false,
  }));
  const [deleting, setDeleting] = createSignal(false);

  return (
    <section class={ui.page}>
      <PageHeader
        title="Computer"
        back={{ href: "/computers", label: "Computers" }}
        badge={<Show when={computer.data}>{(current) => <StatusBadge resource="computer" status={current().status} />}</Show>}
        subtitle={<Show when={computer.data}>{(current) => <IDText value={current().id} mode="full" />}</Show>}
        actions={
          <Show when={computer.data && hasPermission(me.data, "computers.delete")}>
            <button type="button" class={ui.dangerOutlineButton} onClick={() => setDeleting(true)}>Delete</button>
          </Show>
        }
      />

      <Show when={computerID() !== ""} fallback={<StatePanel error="Computer ID is required." />}>
        <Show when={hasScope()} fallback={<StatePanel empty="Select a project and environment." />}>
          <Show when={!computer.isPending} fallback={<StatePanel loading="Loading Computer..." />}>
            <Show
              when={computer.data}
              fallback={
                <StatePanel empty={computerErrorMessage(computer.error)}>
                  <button class={ui.secondaryButton} type="button" onClick={() => void computer.refetch()}>
                    Retry
                  </button>
                </StatePanel>
              }
            >
              {(current) => (
                <div class="grid grid-cols-[minmax(0,1fr)_310px] items-start gap-3.5 max-[960px]:grid-cols-1">
                  <div class="grid gap-3.5">
                    <ComputerMembersPanel computerID={current().id} scope={resourceScope()} />
                    <Panel title="Secret placements">
                      <Show when={current().secrets.length > 0} fallback={<StatePanel empty="No Secrets are attached." />}>
                        <DataTable columns={["Secret", "Target", "Mode", "Origins"]} minWidth="min-w-0">
                          <For each={current().secrets}>
                            {(secret) => (
                              <tr>
                                <td><code>{secret.secret}</code></td>
                                <td><code>{secret.env?.name ?? secret.file?.path}</code></td>
                                <td>{secret.env?.mode === "protected" ? "Protected env" : secret.env ? "Raw env" : "Raw file"}</td>
                                <td>{secret.env?.allowed_origins?.join(", ") ?? "—"}</td>
                              </tr>
                            )}
                          </For>
                        </DataTable>
                      </Show>
                    </Panel>

                    <Show when={hasPermission(me.data, "computer.exec.create")}>
                      <ExecPanel computerID={current().id} scope={resourceScope()} />
                    </Show>
                  </div>

                  <DetailList title="Computer details">
                    <DetailItem label="Status"><StatusBadge resource="computer" status={current().status} /></DetailItem>
                    <DetailItem label="Residency">{current().residency}</DetailItem>
                    <Show when={current().error}>{(error) => <DetailItem label="Unavailable reason">{error().message}</DetailItem>}</Show>
                    <DetailItem label="Key">
                      <Show when={current().key} fallback={<span class="text-console-faint">—</span>}>{(key) => <code>{key()}</code>}</Show>
                    </DetailItem>
                    <DetailItem label="Sandbox"><code>{current().sandbox_id}</code></DetailItem>
                    <DetailItem label="Deployment">
                      <IDText value={current().deployment_id} mode="link" href={deploymentHref(current().deployment_id)} />
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

      <Show when={deleting() && computer.data}>
        {(current) => (
          <ConfirmModal
            title="Delete Computer"
            confirmLabel="Delete Computer"
            busyLabel="Deleting..."
            tone="danger"
            onClose={() => setDeleting(false)}
            onConfirm={async () => {
              await deleteComputer(current().id, resourceScope(), { idempotency_key: crypto.randomUUID() });
              await queryClient.invalidateQueries({ queryKey: ["computers"] });
              navigate("/computers");
            }}
            errorMessage={(error) => actionErrorMessage(error, "Could not delete this Computer.")}
          >
            The Computer filesystem is removed and cannot be recovered. A Computer owned by an open Session or an active Run cannot be deleted. Computer <IDText value={current().id} />.
          </ConfirmModal>
        )}
      </Show>
    </section>
  );
}
