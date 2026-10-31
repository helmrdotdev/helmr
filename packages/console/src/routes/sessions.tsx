import { RelatedCommands } from "../features/sessions/RelatedCommands";
import { useSearchParams } from "@solidjs/router";
import { agentHref, listRetainedAgents } from "../lib/definitions";
import { computerHref } from "../features/computers/navigation";
import { HeldLabel } from "../features/sessions/HeldLabel";
import { SessionOrigin } from "../features/sessions/SessionOrigin";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { loadError } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import {
  holdNotice,
  listSessions,
  sessionConsolePath,
  type Session,
  type SessionAddress,
  type SessionStatus,
} from "../lib/sessions";
import { DataTable, EmptyCell, LoadMore, PrimaryLink, RowActions } from "../ui/DataTable";
import { Notice } from "../ui/Notice";
import type { ActionMenuItem } from "../ui/ActionMenu";
import { useCopyLink } from "../components/copyLink";
import { createSessionControls } from "../features/sessions/sessionControls";
import { hasPermission } from "../lib/auth";
import { useMe } from "../lib/me";
import { IDText } from "../ui/IDText";
import { Toolbar } from "../ui/Page";
import { QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { Select, type SelectOption } from "../ui/Select";
import { StatusBadge } from "../ui/StatusBadge";
import { useAuditItem } from "../features/audit/auditLink";

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

function SessionRow(props: { session: Session; agentColumn: boolean; menu: ActionMenuItem[] }) {
  return (
    <tr>
      <td><IDText value={props.session.id} mode="link" href={sessionConsolePath(props.session.id)} /></td>
      <td>
        <span class="inline-flex items-center gap-2">
          <StatusBadge resource="session" status={props.session.status} />
          <Show when={props.session.holds.length > 0}><HeldLabel title={props.session.holds.map(hold => holdNotice(hold, props.session.id)).join("\n")} /></Show>
        </span>
      </td>
      <Show when={props.agentColumn}>
        <td><PrimaryLink href={agentHref(props.session.agent)}>{props.session.agent}</PrimaryLink></td>
      </Show>
      <td><SessionOrigin session={props.session} /><Show when={props.session.related_via}><div class="text-chrome text-console-muted">{{initiated:"Started by this person",contributed:"Received input from this person",answered:"Question answered by this person",descendant:"Started by related work"}[props.session.related_via!]}</div></Show></td>
      <td>
        <Show when={props.session.computer_id} fallback={<EmptyCell />}>
          {(id) => <IDText value={id()} mode="link" href={computerHref(id())} />}
        </Show>
      </td>
      <td><RelativeTime value={props.session.created_at} /></td>
      <RowActions label={`Actions for ${props.session.id}`} items={props.menu} />
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
  const [params, setParams] = useSearchParams<{ agent?: string; user_id?: string }>();
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
    queryKey: ["sessions", "list", filter(), projectID(), environmentID(), agent(),params.user_id],
    queryFn: ({ pageParam }) => {
      const status = filter();
      return listSessions({
        projectID: projectID(),
        environmentID: environmentID(),
        agent: agent() || undefined,
        relatedUserID:params.user_id,
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
    ? ["Session", "Status", "Started from", "Computer", "Created", { label: "Actions", srOnly: true }]
    : ["Session", "Status", "Agent", "Started from", "Computer", "Created", { label: "Actions", srOnly: true }];
  // One control set for the list; a row's menu points it at that row.
  const me = useMe();
  const copyLink = useCopyLink();
  const auditItem = useAuditItem();
  const [target, setTarget] = createSignal<SessionAddress>({ sessionID: "", projectID: "", environmentID: "" });
  const [receipt, setReceipt] = createSignal<string>();
  const controls = createSessionControls({ address: target, onChanged: () => sessions.refetch(), onReceipt: setReceipt });
  const menu = (session: Session): ActionMenuItem[] => {
    const can = (permission: string) => session.permissions?.includes(permission) ?? hasPermission(me.data, permission, { ...scope(), agent: session.agent });
    const point = (item: ActionMenuItem): ActionMenuItem => item.onSelect ? { ...item, onSelect: () => { setTarget({ sessionID: session.id, projectID: projectID(), environmentID: environmentID() }); item.onSelect!(); } } : item;
    return [...controls.items({ live: session.status === "open" || session.status === "closing", open: session.status === "open", can, copyLink: copyLink.item(sessionConsolePath(session.id)) }).map(point), ...auditItem({ target: session.id, target_kind: "session" })];
  };

  return (
    <>
      <copyLink.LiveRegion />
      <Show when={params.user_id}><Notice tone="info" class="mb-3">Related work for <IDText value={params.user_id!}/> in the selected environment. Access revocation does not stop this work. Use each row's separate controls; only work you can view is listed. Future scheduled runs remain active until their schedule is changed. <button type="button" class="underline" onClick={()=>setParams({user_id:undefined})}>Clear filter</button></Notice></Show>
      <Show when={receipt()}>{text => <Notice tone="info" class="mb-3" onDismiss={() => setReceipt(undefined)}>{text()}</Notice>}</Show>
      <Show when={controls.error()}>{text => <Notice tone="danger" class="mb-3" role="alert" onDismiss={controls.clearError}>{text()}</Notice>}</Show>
      <Toolbar
        start={
          <>
            <Select<SessionFilter> value={filter()} options={FILTERS} onChange={setFilter} ariaLabel="Filter by status" />
            <Show when={!fixedAgent()}>
              <Select<string> value={agent()} options={agentOptions()} onChange={(value) => setParams({ agent: value || undefined })} ariaLabel="Filter by agent" />
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
            <For each={items()}>{(session) => <SessionRow session={session} agentColumn={!fixedAgent()} menu={menu(session)} />}</For>
          </DataTable>
        )}
      </QueryState>
      <Show when={params.user_id}>{user=><RelatedCommands user={user()} />}</Show>
      <controls.Dialogs />
    </>
  );
}

