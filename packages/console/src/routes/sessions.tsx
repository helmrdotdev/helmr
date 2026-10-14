import { A, useSearchParams } from "@solidjs/router";
import { definitionHref } from "../lib/definitions";
import { computerHref } from "../features/computers/navigation";
import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { errorMessage } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import {
  listSessions,
  sessionConsolePath,
  type Session,
  type SessionStatus,
} from "../lib/sessions";
import { DataTable, EmptyCell, LoadMore, PrimaryLink } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Notice } from "../ui/Notice";
import { Page, Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { Select, type SelectOption } from "../ui/Select";
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

const sessionsErrorMessage = (error: unknown) =>
  errorMessage(error, "Could not load Sessions.", { forbidden: "You do not have permission to view Sessions." });

function SessionRow(props: { session: Session }) {
  return (
    <tr>
      <td><IDText value={props.session.id} mode="link" href={sessionConsolePath(props.session.id)} /></td>
      <td><PrimaryLink href={definitionHref("agents", props.session.agent, props.session.deployment_id)}>{props.session.agent}</PrimaryLink></td>
      <td>
        <Show when={props.session.key} fallback={<EmptyCell />}>
          {(key) => <code>{key()}</code>}
        </Show>
      </td>
      <td><StatusBadge resource="session" status={props.session.status} /></td>
      <td>{props.session.holds.length ? `${props.session.holds.length} active` : "None"}</td>
      <td>
        <Show when={props.session.computer_id} fallback={<EmptyCell />}>
          {(id) => <IDText value={id()} mode="link" href={computerHref(id())} />}
        </Show>
      </td>
      <td><RelativeTime value={props.session.created_at} /></td>
    </tr>
  );
}

export function Sessions() {
  const scope = useScopeIDs();
  const [search] = useSearchParams();
  const text = (v: string | string[] | undefined) => typeof v === "string" ? v : "";
  const projectID = () => scope().projectID;
  const environmentID = () => scope().environmentID;
  const agent = () => text(search["agent"]);
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

  return (
    <Page>
      <PageHeader title="Sessions" subtitle="Agent Sessions and retained work in the selected environment." />
      <Toolbar
        start={
          <div class="w-44">
            <Select<SessionFilter> value={filter()} options={FILTERS} onChange={setFilter} ariaLabel="Filter sessions" />
          </div>
        }
      />
      <Show when={agent()}>
        <Notice tone="info" role="none" class="mb-3" action={<A class={ui.monoLink} href="/sessions">Show all Sessions</A>}>
          Showing this Agent’s Sessions across all Deployments.
        </Notice>
      </Show>
      <QueryState
        query={sessions}
        pending="Loading Sessions..."
        error={sessionsErrorMessage}
        empty={{
          when: (data) => data.pages.every((page) => page.sessions.length === 0),
          title: filter() === "all" ? "No Sessions yet." : "No Sessions match this filter.",
          hint: filter() === "all" ? "Starting an Agent creates a Session." : undefined,
        }}
      >
        {() => (
          <DataTable
            columns={["Session", "Agent", "Key", "Status", "Holds", "Computer", "Created"]}
            minWidth="min-w-200"
            footer={<LoadMore count={items().length} noun="Sessions" hasMore={sessions.hasNextPage} loading={sessions.isFetchingNextPage} failed={sessions.isFetchNextPageError} onLoadMore={() => void sessions.fetchNextPage()} />}
          >
            <For each={items()}>{(session) => <SessionRow session={session} />}</For>
          </DataTable>
        )}
      </QueryState>
    </Page>
  );
}
