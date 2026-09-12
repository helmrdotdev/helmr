import { A } from "@solidjs/router";
import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { runHref } from "../features/runs/navigation";
import { SessionStatusBadge } from "../features/sessions/display";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import { listSessions, sessionConsolePath, type Session } from "../lib/sessions";
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
      <td><SessionStatusBadge status={props.session.status} /></td>
      <td>
        <Show when={props.session.current_run_id} fallback={<span class="text-console-faint">—</span>}>
          {(runID) => (
            <A
              href={runHref(runID(), props.projectID, props.environmentID)}
              class="font-mono text-[11.5px] text-console-accent hover:text-console-accent-hover"
            >
              {runID().slice(0, 12)}
            </A>
          )}
        </Show>
      </td>
      <td><span class={ui.muted}>{formatRelative(props.session.created_at)}</span></td>
      <td><code>{props.session.id.slice(0, 12)}</code></td>
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
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Sessions</h1>
          <p class={ui.pageSubtitle}>
            Actor Sessions in the selected environment, with the Run currently serving each one.
          </p>
        </div>
      </div>

      <Show when={sessions.isError}>
        <p class={ui.error} role="alert">{sessionsErrorMessage(sessions.error)}</p>
      </Show>
      <Show when={!sessions.isPending} fallback={<p class={ui.muted}>Loading Sessions...</p>}>
        <Show
          when={items().length > 0}
          fallback={
            <div class={ui.emptyState}>
              <strong>No Sessions yet.</strong>
              <span>Starting a declared Actor creates a Session.</span>
            </div>
          }
        >
          <div class={ui.tableWrap}>
            <table class="min-w-200">
              <thead>
                <tr>
                  <th>Actor</th>
                  <th>Key</th>
                  <th>Status</th>
                  <th>Current run</th>
                  <th>Created</th>
                  <th>Session</th>
                </tr>
              </thead>
              <tbody>
                <For each={items()}>
                  {(session) => <SessionRow session={session} projectID={projectID()} environmentID={environmentID()} />}
                </For>
              </tbody>
            </table>
          </div>
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
