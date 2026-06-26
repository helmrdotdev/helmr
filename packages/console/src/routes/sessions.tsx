import { A, useSearchParams } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { Select, type SelectOption } from "../ui/Select";
import { formatRelative } from "../features/runs/display";
import { SessionActivityBadge, SessionStatusBadge, type SessionStatus } from "../features/sessions/display";
import { sessionHref, useSessionRowNavigation } from "../features/sessions/navigation";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import { listSessions, type ListSessionsOptions, type Session } from "../lib/sessions";
import { ui } from "../ui/styles";

type SessionFilter = SessionStatus | "all";

const FILTER_OPTIONS: SelectOption<SessionFilter>[] = [
  { value: "open", label: "Open" },
  { value: "all", label: "All" },
  { value: "closed", label: "Closed" },
  { value: "cancelled", label: "Cancelled" },
  { value: "expired", label: "Expired" },
];

function sessionsErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.errorKind === "forbidden") {
    return "You do not have permission to view sessions.";
  }
  return "Could not load sessions.";
}

function shortID(id: string | undefined): string {
  return id ? id.slice(0, 8) : "—";
}

function SessionRow(props: { session: Session }) {
  const rowNavigation = useSessionRowNavigation(() => props.session);

  return (
    <tr class={ui.clickableTableRow} {...rowNavigation}>
      <td>
        <A
          href={sessionHref(props.session.id, props.session.project_id, props.session.environment_id)}
          class={"cursor-pointer font-medium text-console-text hover:text-console-accent"}
        >
          {props.session.task_id}
        </A>
      </td>
      <td><SessionStatusBadge status={props.session.status} /></td>
      <td><SessionActivityBadge activity={props.session.activity} /></td>
      <td><code>{shortID(props.session.workspace_id)}</code></td>
      <td><code>{shortID(props.session.current_run_id)}</code></td>
      <td><code>{props.session.external_id || "—"}</code></td>
      <td><span class={ui.muted}>{formatRelative(props.session.updated_at)}</span></td>
      <td><code>{shortID(props.session.id)}</code></td>
    </tr>
  );
}

function SessionsEmptyState(props: { filtered: boolean }) {
  return (
    <div class={ui.emptyState}>
      <strong class="text-console-text">
        {props.filtered ? "No sessions match this filter." : "No sessions yet."}
      </strong>
      <span>Started sessions will appear here with their workspace and run history.</span>
    </div>
  );
}

function searchParamValue(value: string | string[] | undefined): string {
  return typeof value === "string" ? value.trim() : "";
}

export function Sessions() {
  const scope = useScope();
  const [searchParams] = useSearchParams();
  const [filter, setFilter] = createSignal<SessionFilter>("all");
  const taskID = createMemo(() => searchParamValue(searchParams["task_id"]));
  const sessionOptions = (): ListSessionsOptions => {
    const options: ListSessionsOptions = {
      status: filter(),
      projectID: scope.selectedProjectID(),
      environmentID: scope.selectedEnvironmentID(),
    };
    if (taskID()) options.taskID = taskID();
    return options;
  };
  const sessions = createQuery(() => ({
    queryKey: ["sessions", filter(), taskID(), scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () => listSessions(sessionOptions()),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));
  const sessionItems = createMemo(() => sessions.data?.sessions ?? []);

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Sessions</h1>
          <p class={ui.pageSubtitle}>
            Task invocation history for the selected environment. Run attempts are available from each session.
          </p>
        </div>
      </div>

      <div class={ui.toolbar}>
        <div class={ui.toolbarSide}>
          <Show when={taskID()}>
            {(currentTaskID) => (
              <span class={ui.filterField}>
                <span>Task</span>
                <code>{currentTaskID()}</code>
                <A href="/sessions" class={ui.ghostButton}>Clear</A>
              </span>
            )}
          </Show>
        </div>
        <div class={ui.toolbarSide}>
          <span class={ui.filterField}>
            <span>Filter</span>
            <Select<SessionFilter>
              value={filter()}
              options={FILTER_OPTIONS}
              onChange={setFilter}
              ariaLabel="Filter sessions"
              minWidth="150px"
            />
          </span>
        </div>
      </div>

      <Show when={sessions.isError}>
        <p class={ui.error} role="alert">{sessionsErrorMessage(sessions.error)}</p>
      </Show>

      <Show when={!sessions.isPending} fallback={<p class={ui.muted}>Loading sessions...</p>}>
        <Show
          when={sessionItems().length > 0}
          fallback={<SessionsEmptyState filtered={filter() !== "all" || taskID() !== ""} />}
        >
          <div class={ui.tableWrap}>
            <table class={ui.dataTable}>
              <thead>
                <tr>
                  <th>Task</th>
                  <th>Status</th>
                  <th>Activity</th>
                  <th>Workspace</th>
                  <th>Current run</th>
                  <th>External ID</th>
                  <th>Updated</th>
                  <th>ID</th>
                </tr>
              </thead>
              <tbody>
                <For each={sessionItems()}>
                  {(session) => <SessionRow session={session} />}
                </For>
              </tbody>
            </table>
          </div>
        </Show>
      </Show>
    </section>
  );
}
