import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import {
  listSessions,
  sessionConsolePath,
  type Session,
  type SessionStatus,
} from "../lib/sessions";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { Select, type SelectOption } from "../ui/Select";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

type SessionFilter = SessionStatus | "all";

const FILTERS: SelectOption<SessionFilter>[] = [
  { value: "all", label: "All sessions" },
  { value: "open", label: "Open" },
  { value: "closed", label: "Closed" },
  { value: "closing", label: "Closing" },
  { value: "cancelled", label: "Cancelled" },
];

function sessionsErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "forbidden") {
    return "You do not have permission to view Sessions.";
  }
  if (error instanceof ApiError) return error.message;
  return "Could not load Sessions.";
}

function SessionRow(props: {
  session: Session;
  projectID: string;
  environmentID: string;
}) {
  return (
    <tr>
      <td>
        <IDText value={props.session.id} mode="link"
          href={sessionConsolePath(props.session.id, props.projectID, props.environmentID)} />
      </td>
      <td><IDText value={props.session.agent_id} /></td>
      <td>
        <Show
          when={props.session.key}
          fallback={<span class="text-console-faint">—</span>}
        >
          {(key) => <code>{key()}</code>}
        </Show>
      </td>
      <td>
        <StatusBadge resource="session" status={props.session.status} />
      </td>
      <td>{props.session.holds.length ? `${props.session.holds.length} active` : "None"}</td>
      <td>
        <IDText
          value={props.session.computer_id}
          mode="link"
          href={`/computers/${props.session.computer_id}`}
        />
      </td>
      <td>
        <RelativeTime value={props.session.created_at} />
      </td>
    </tr>
  );
}

export function Sessions() {
  const scope = useScope();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const [filter, setFilter] = createSignal<SessionFilter>("all");
  const sessions = createInfiniteQuery(() => ({
    queryKey: ["sessions", "list", filter(), projectID(), environmentID()],
    queryFn: ({ pageParam }) => {
      const status = filter();
      return listSessions({
        projectID: projectID(),
        environmentID: environmentID(),
        statuses: status === "all" ? undefined : [status],
        cursor: pageParam || undefined,
        limit: 100,
      });
    },
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    enabled: !!projectID() && !!environmentID(),
    retry: false,
    refetchInterval: 5_000,
  }));
  const items = createMemo(
    () => sessions.data?.pages.flatMap((page) => page.sessions) ?? [],
  );

  return (
    <section class={ui.page}>
      <PageHeader
        title="Sessions"
        subtitle="Agent Sessions and retained work in the selected environment."
        actions={
          <div class="w-44">
            <Select<SessionFilter>
              value={filter()}
              options={FILTERS}
              onChange={setFilter}
              ariaLabel="Filter sessions"
            />
          </div>
        }
      />

      <Show when={sessions.isError}>
        <StatePanel error={sessionsErrorMessage(sessions.error)} />
      </Show>
      <Show
        when={!sessions.isPending}
        fallback={<StatePanel loading="Loading Sessions..." />}
      >
        <Show
          when={items().length > 0}
          fallback={
            <Show
              when={filter() === "all"}
              fallback={<StatePanel empty="No Sessions match this filter." />}
            >
              <StatePanel
                empty="No Sessions yet."
                hint="Starting an Agent creates a Session."
              />
            </Show>
          }
        >
          <DataTable
            columns={["Session", "Agent", "Key", "Status", "Holds", "Computer", "Created"]}
            minWidth="min-w-200"
          >
            <For each={items()}>
              {(session) => (
                <SessionRow
                  session={session}
                  projectID={projectID()}
                  environmentID={environmentID()}
                />
              )}
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
