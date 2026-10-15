import { useSearchParams } from "@solidjs/router";
import { agentHref, listRetainedAgents } from "../lib/definitions";
import { computerHref } from "../features/computers/navigation";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { loadError } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import {
  listSessions,
  sessionConsolePath,
  type Session,
  type SessionStatus,
} from "../lib/sessions";
import { DataTable, EmptyCell, LoadMore, PrimaryLink } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Page, Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { Select, type SelectOption } from "../ui/Select";
import { StatusBadge } from "../ui/StatusBadge";

type SessionFilter = SessionStatus | "all";

const FILTERS: SelectOption<SessionFilter>[] = [
  { value: "all", label: "All sessions" },
  { value: "open", label: "Open" },
  { value: "closed", label: "Closed" },
  { value: "closing", label: "Closing" },
  { value: "cancelled", label: "Cancelled" },
];

const sessionsErrorMessage = (error: unknown) =>
  loadError(error, "Could not load sessions.", { forbidden: "You do not have permission to view sessions." });

function SessionRow(props: { session: Session; agentColumn: boolean }) {
  return (
    <tr>
      <td><IDText value={props.session.id} mode="link" href={sessionConsolePath(props.session.id)} /></td>
      <td><StatusBadge resource="session" status={props.session.status} /></td>
      <Show when={props.agentColumn}>
        <td><PrimaryLink href={agentHref(props.session.agent)}>{props.session.agent}</PrimaryLink></td>
      </Show>
      <td>
        <Show when={props.session.key} fallback={<EmptyCell />}>
          {(key) => <code>{key()}</code>}
        </Show>
      </td>
      <td><Show when={props.session.holds.length} fallback={<EmptyCell />}>{(count) => `${count()} active`}</Show></td>
      <td>
        <Show when={props.session.computer_id} fallback={<EmptyCell />}>
          {(id) => <IDText value={id()} mode="link" href={computerHref(id())} />}
        </Show>
      </td>
      <td><RelativeTime value={props.session.created_at} /></td>
    </tr>
  );
}

// The Sessions of the tab's Environment, or of one Agent across all its
// Deployments.
export function SessionList(props: { agent?: string }) {
  const scope = useScopeIDs();
  const projectID = () => scope().projectID;
  const environmentID = () => scope().environmentID;
  // On an Agent's page the Agent is fixed; on the Sessions page it is a filter
  // kept in the URL (`?agent=`), so a link can open the list already filtered.
  const [params, setParams] = useSearchParams<{ agent?: string }>();
  const fixedAgent = () => props.agent !== undefined;
  const agent = () => props.agent ?? params.agent ?? "";
  const agents = createQuery(() => ({
    // The first 100 retained Agents; any other can still be named in the URL.
    queryKey: ["agents", "palette", projectID(), environmentID()],
    queryFn: () => listRetainedAgents(scope()),
    enabled: !fixedAgent() && !!projectID() && !!environmentID(),
    retry: false,
  }));
  const agentOptions = (): SelectOption<string>[] => [
    { value: "", label: "All agents" },
    ...(agents.data?.agents ?? []).map((item) => ({ value: item.agent, label: item.agent })),
    ...(agent() && !agents.data?.agents.some((item) => item.agent === agent()) ? [{ value: agent(), label: agent() }] : []),
  ];
  const [filter, setFilter] = createSignal<SessionFilter>("all");
  const sessions = createInfiniteQuery(() => ({
    queryKey: ["sessions", "list", filter(), projectID(), environmentID(), agent()],
    queryFn: ({ pageParam }) => {
      const status = filter();
      return listSessions({
        projectID: projectID(),
        environmentID: environmentID(),
        agent: agent() || undefined,
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

  const columns = () => fixedAgent()
    ? ["Session", "Status", "Key", "Holds", "Computer", "Created"]
    : ["Session", "Status", "Agent", "Key", "Holds", "Computer", "Created"];

  return (
    <>
      <Toolbar
        start={
          <>
            <div class="w-44">
              <Select<SessionFilter> value={filter()} options={FILTERS} onChange={setFilter} ariaLabel="Filter by status" />
            </div>
            <Show when={!fixedAgent()}>
              <div class="w-44">
                <Select<string> value={agent()} options={agentOptions()} onChange={(value) => setParams({ agent: value || undefined })} ariaLabel="Filter by agent" />
              </div>
            </Show>
          </>
        }
      />
      <QueryState
        query={sessions}
        pending={<TableSkeleton label="Loading sessions" columns={columns()} minWidth="min-w-200" />}
        error={sessionsErrorMessage}
        empty={{
          when: (data) => data.pages.every((page) => page.sessions.length === 0),
          title: filter() !== "all" ? "No sessions match this filter." : agent() ? "This agent has no sessions yet." : "No sessions yet.",
          hint: filter() === "all" ? "Starting an agent creates a session." : undefined,
        }}
      >
        {() => (
          <DataTable
            columns={columns()}
            minWidth="min-w-200"
            footer={<LoadMore count={items().length} noun={{ one: "session", other: "sessions" }} hasMore={sessions.hasNextPage} loading={sessions.isFetchingNextPage} failed={sessions.isFetchNextPageError} onLoadMore={() => void sessions.fetchNextPage()} />}
          >
            <For each={items()}>{(session) => <SessionRow session={session} agentColumn={!fixedAgent()} />}</For>
          </DataTable>
        )}
      </QueryState>
    </>
  );
}

// The Sessions page: every Session in the Environment, filtered by status and
// Agent.
export function SessionsPage() {
  const scope = useScopeIDs();
  return (
    <Page>
      <PageHeader title="Sessions" />
      <Show when={scope().projectID && scope().environmentID} fallback={<EmptyState title="Select a project and environment." />}>
        <SessionList />
      </Show>
    </Page>
  );
}
