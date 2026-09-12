import { A } from "@solidjs/router";
import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { runHref } from "../features/runs/navigation";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import { listSessions, sessionConsolePath, type Session } from "../lib/sessions";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

function sessionsErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "forbidden") {
    return "You do not have permission to view Sessions.";
  }
  if (error instanceof ApiError) return error.message;
  return "Could not load Sessions.";
}

function SessionRow(props: { session: Session; projectID: string; environmentID: string }) {
  return (
    <tr>
      <td>
        <A
          href={sessionConsolePath(props.session.id, props.projectID, props.environmentID)}
          class="font-medium text-console-text hover:text-console-accent"
        >
          {props.session.actor_id}
        </A>
      </td>
      <td>
        <Show when={props.session.key} fallback={<span class="text-console-faint">—</span>}>
          {(key) => <code>{key()}</code>}
        </Show>
      </td>
      <td><StatusBadge resource="session" status={props.session.status} /></td>
      <td>
        <Show when={props.session.current_run_id} fallback={<span class="text-console-faint">—</span>}>
          {(runID) => (
            <A href={runHref(runID(), props.projectID, props.environmentID)} class="text-console-accent hover:text-console-accent-hover">
              <IDText value={runID()} class="text-inherit hover:text-inherit" />
            </A>
          )}
        </Show>
      </td>
      <td><RelativeTime value={props.session.created_at} /></td>
      <td><IDText value={props.session.id} /></td>
    </tr>
  );
}

export function Sessions() {
  const scope = useScope();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const sessions = createInfiniteQuery(() => ({
    queryKey: ["sessions", "list", projectID(), environmentID()],
    queryFn: ({ pageParam }) => listSessions({
      projectID: projectID(),
      environmentID: environmentID(),
      cursor: pageParam || undefined,
      limit: 100,
    }),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    enabled: !!projectID() && !!environmentID(),
    retry: false,
  }));
  const items = createMemo(() => sessions.data?.pages.flatMap((page) => page.sessions) ?? []);

  return (
    <section class={ui.page}>
      <PageHeader
        title="Sessions"
        subtitle="Actor Sessions in the selected environment, with the Run currently serving each one."
      />

      <Show when={sessions.isError}>
        <StatePanel error={sessionsErrorMessage(sessions.error)} />
      </Show>
      <Show when={!sessions.isPending} fallback={<StatePanel loading="Loading Sessions..." />}>
        <Show
          when={items().length > 0}
          fallback={<StatePanel empty="No Sessions yet." hint="Starting a declared Actor creates a Session." />}
        >
          <DataTable columns={["Actor", "Key", "Status", "Current run", "Created", "Session"]} minWidth="min-w-200">
            <For each={items()}>
              {(session) => <SessionRow session={session} projectID={projectID()} environmentID={environmentID()} />}
            </For>
          </DataTable>
          <Show when={sessions.hasNextPage}>
            <div class={ui.actionRow}>
              <button
                type="button"
                class={ui.secondaryButton}
                disabled={sessions.isFetchingNextPage}
                onClick={() => void sessions.fetchNextPage()}
              >
                {sessions.isFetchingNextPage ? "Loading..." : "Load more"}
              </button>
            </div>
          </Show>
        </Show>
      </Show>
    </section>
  );
}
