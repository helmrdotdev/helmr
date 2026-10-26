import { useSearchParams } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, For, Match, on, Show, Switch, type JSX } from "solid-js";
import { AuditEventDetails } from "../features/audit/AuditEventDetails";
import { canReadAudit } from "../features/audit/auditLink";
import { listAuditActions, searchAuditEvents, type AuditBefore, type AuditEvent, type AuditFilter } from "../lib/audit";
import { actorOf, CATEGORY_LABELS, eventSummary, type ActorKind, type AuditNames } from "../lib/auditText";
import { hasPermission } from "../lib/auth";
import { errorMessage } from "../lib/error";
import { useMe } from "../lib/me";
import { listMembers } from "../lib/members";
import { listAccessTargets, listGroups, listRoles, memberHref } from "../lib/permissions";
import { useScope } from "../lib/scope";
import { DataTable, LoadMore } from "../ui/DataTable";
import { Segmented } from "../ui/Field";
import { Icon, type IconName } from "../ui/Icon";
import { PageHeader } from "../ui/PageHeader";
import { DeniedState, EmptyState, isDenied } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { Select } from "../ui/Select";
import { TableSkeleton } from "../ui/Skeleton";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, ui } from "../ui/styles";

const RANGES = [
  { value: "1h", label: "Last hour", ms: 3_600_000 },
  { value: "24h", label: "Last 24 hours", ms: 86_400_000 },
  { value: "7d", label: "Last 7 days", ms: 7 * 86_400_000 },
  { value: "31d", label: "Last 31 days", ms: 31 * 86_400_000 },
] as const;
type Range = (typeof RANGES)[number]["value"];

const OUTCOMES = [
  { value: "", label: "All" },
  { value: "denied", label: "Denied" },
  { value: "failed", label: "Failed" },
] as const;

const ACTOR_ICONS: Partial<Record<ActorKind, IconName>> = { member: "member", api_key: "api-key", session: "terminal" };
const COLUMNS = [{ label: "Details", srOnly: true }, "When", "Who", "What", "Outcome", "Where"] as const;

type Params = { range?: string; outcome?: string; category?: string; actor?: string; actor_kind?: string; target?: string; target_kind?: string; environment_id?: string; operation?: string; event?: string; focus?: string; at?: string };

const DAY = 86_400_000;

// A filter that came from a link, with a button that clears it.
function Chip(props: { label: string; onClear: () => void; children: JSX.Element }) {
  return (
    <span class="inline-flex items-center gap-1.5 rounded-xs border border-console-info-border bg-console-info-bg px-2 py-0.5 text-console-info">
      {props.children}
      <button type="button" class="cursor-pointer border-0 bg-transparent p-0 text-console-info" aria-label={props.label} onClick={() => props.onClear()}>✕</button>
    </span>
  );
}

// Following an operation or a cause shows all of it, whatever the list was
// filtered on.
const CLEAR_PREDICATES = { outcome: undefined, category: undefined, actor: undefined, actor_kind: undefined, target: undefined, target_kind: undefined, environment_id: undefined };

// The time a search covers: the chosen range up to now, or the 30 days up to
// an event a link or a cause names (the API allows at most 31).
function timeWindow(params: Params): { from: string; to: string } {
  if ((params.focus || params.operation) && params.at && !Number.isNaN(Date.parse(params.at))) {
    const at = Date.parse(params.at);
    return params.operation ? { from: new Date(at - 3_600_000).toISOString(), to: new Date(at + 3_600_000).toISOString() } : { from: new Date(at - 30 * DAY).toISOString(), to: new Date(at + 1000).toISOString() };
  }
  const range = RANGES.find((r) => r.value === params.range) ?? RANGES[1];
  const to = Date.now() + 60_000;
  return { from: new Date(to - range.ms).toISOString(), to: new Date(to).toISOString() };
}

// Investigate recorded security activity: newest events first, filtered by
// time, outcome, kind of action, who acted, what was acted on and where. A row
// opens beneath itself to show the event's recorded fields. Filters live in the URL, so a link
// reproduces the same view.
export function AuditEvents(props: { installation?: boolean }) {
  const me = useMe();
  const scope = useScope();
  const [params, setParams] = useSearchParams<Params>();
  const allowed = createMemo(() => (props.installation ? !!me.data?.admin : canReadAudit(me.data)));
  const can = (permission: string) => hasPermission(me.data, permission);

  const actions = createQuery(() => ({ queryKey: ["audit", "actions"], queryFn: listAuditActions, enabled: allowed(), staleTime: Infinity, retry: false }));
  const members = createQuery(() => ({ queryKey: ["members"], queryFn: listMembers, enabled: allowed() && can("members.read"), retry: false }));
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles, enabled: allowed() && can("roles.read"), retry: false }));
  const groups = createQuery(() => ({ queryKey: ["access", "groups"], queryFn: listGroups, enabled: allowed() && can("groups.read"), retry: false }));
  const targets = createQuery(() => ({ queryKey: ["access", "targets", scope.selectedProjectID()], queryFn: () => listAccessTargets(scope.selectedProjectID()), enabled: allowed() && !!scope.selectedProjectID(), retry: false }));

  // Every name the viewer can already see, by ID.
  const names = createMemo<AuditNames>(() => {
    const map = new Map<string, string>();
    for (const m of members.data?.members ?? []) { map.set(m.user_id, m.display_name || m.email || m.user_id); map.set(m.membership_id, m.display_name || m.email || m.membership_id); }
    for (const r of roles.data?.roles ?? []) map.set(r.id, r.name);
    for (const g of groups.data?.groups ?? []) map.set(g.id, g.name);
    for (const t of targets.data?.targets ?? []) { map.set(t.environment_id, t.name); for (const a of t.agents) map.set(a.id, a.key); }
    return { get: (id) => map.get(id) };
  });
  const memberLink = (userID: string) => {
    const member = (members.data?.members ?? []).find((m) => m.user_id === userID && m.status === "active");
    return member ? memberHref(member.membership_id) : undefined;
  };
  const rolesWith = (permission: string) => (roles.data?.roles ?? []).filter((r) => r.permissions.includes(permission)).map((r) => ({ id: r.id, name: r.name }));

  const inCategory = createMemo(() => (params.category ? (actions.data?.actions ?? []).filter((a) => a.category === params.category).map((a) => a.action) : []));
  // What the search depends on. Opening a row changes only that row, so it
  // keeps the loaded pages and their time window.
  const searchKey = createMemo(() => JSON.stringify([params.range, params.outcome, inCategory(), params.actor, params.actor_kind, params.target, params.target_kind, params.environment_id, params.operation, params.focus, params.at]));
  const filter = createMemo(on(searchKey, (): AuditFilter => {
    return {
      ...timeWindow(params),
      actions: inCategory(),
      outcome: params.outcome ?? "",
      actor_id: params.actor ?? "",
      actor_kind: params.actor_kind ?? "",
      target_id: params.target ?? "",
      target_kind: params.target_kind ?? "",
      environment_id: params.environment_id ?? "",
      operation_id: params.operation ?? "",
      event_id: params.focus && params.at ? params.focus : "",
    };
  }));
  // A category needs its actions before the search can name them.
  const ready = () => allowed() && (!params.category || inCategory().length > 0);
  const events = createInfiniteQuery(() => ({
    queryKey: ["audit", "events", props.installation ?? false, filter()],
    queryFn: ({ pageParam }) => searchAuditEvents(filter(), pageParam, props.installation),
    initialPageParam: undefined as AuditBefore | undefined,
    getNextPageParam: (page) => page.next_before,
    enabled: ready(),
    retry: false,
  }));
  const rows = createMemo(() => events.data?.pages.flatMap((page) => page.events) ?? []);

  // After following an operation or a cause the list is new; keep the keyboard
  // on the opened event's toggle.
  let refocus = false;
  createEffect(on(rows, () => {
    if (!refocus || !params.event) return;
    const toggle = document.querySelector<HTMLElement>(`[data-audit-toggle="${CSS.escape(params.event)}"]`);
    if (toggle) { toggle.focus(); refocus = false; }
  }));

  // Opening a row keeps the list as it is; only a link or a cause narrows the
  // search to one event (`focus`).
  const select = (event: AuditEvent | undefined) => setParams({ event: event?.id });
  const link = (event: AuditEvent) => {
    const url = new URL(window.location.href);
    url.searchParams.set("event", event.id);
    url.searchParams.set("focus", event.id);
    url.searchParams.set("at", event.recorded_at);
    return url.toString();
  };
  const categories = createMemo(() => [
    { value: "", label: "Any action" },
    ...Object.entries(CATEGORY_LABELS).filter(([key]) => (actions.data?.actions ?? []).some((a) => a.category === key)).map(([value, label]) => ({ value, label })),
  ]);
  const who = createMemo(() => [
    { value: "", label: "Anyone" },
    { value: "kind:user", label: "Any member" },
    { value: "kind:api_key", label: "Any API key" },
    { value: "kind:session", label: "Any agent session" },
    { value: "kind:component", label: "Helmr" },
    ...(members.data?.members ?? []).filter((m) => m.status === "active").map((m) => ({ value: m.user_id, label: m.display_name || m.email || m.user_id })),
  ].concat(params.actor && !(members.data?.members ?? []).some((m) => m.user_id === params.actor) ? [{ value: params.actor, label: names().get(params.actor) ?? "Selected actor" }] : []));
  const whoValue = () => (params.actor ? params.actor : params.actor_kind ? `kind:${params.actor_kind}` : "");
  const environments = createMemo(() => [
    { value: "", label: "All environments" },
    ...(targets.data?.targets ?? []).map((t) => ({ value: t.environment_id, label: t.name })),
  ]);
  const targetName = () => (params.target ? names().get(params.target) ?? params.target_kind ?? "this object" : undefined);

  return (
    <div class="min-w-0">
        <PageHeader title={props.installation ? "Installation audit" : "Audit events"} subtitle="Who did what, what Helmr allowed or refused, and why. Searches are recorded; conversation content and command output never are." />
        <Show when={allowed()} fallback={<Show when={!me.isPending}><DeniedState message="You need Read audit events permission for an organization or environment." /></Show>}>
          <div class={ui.toolbar}>
            <div class={ui.toolbarSide}>
              <Select<Range> ariaLabel="Time range" value={(params.range as Range) ?? "24h"} options={RANGES.map(({ value, label }) => ({ value, label }))} onChange={(value) => setParams({ range: value === "24h" ? undefined : value, event: undefined, at: undefined })} />
              <Segmented label="Outcome" value={params.outcome ?? ""} options={OUTCOMES} onChange={(value) => setParams({ outcome: value || undefined, event: undefined, at: undefined })} />
              <Select<string> ariaLabel="Action" value={params.category ?? ""} options={categories()} onChange={(value) => setParams({ category: value || undefined, event: undefined, at: undefined })} />
              <Select<string> ariaLabel="Who" value={whoValue()} options={who()} onChange={(value) => setParams(value.startsWith("kind:") ? { actor_kind: value.slice(5), actor: undefined, event: undefined, at: undefined } : { actor: value || undefined, actor_kind: undefined, event: undefined, at: undefined })} />
              <Show when={!props.installation && environments().length > 1}>
                <Select<string> ariaLabel="Environment" value={params.environment_id ?? ""} options={environments()} onChange={(value) => setParams({ environment_id: value || undefined, event: undefined, at: undefined })} />
              </Show>
            </div>
          </div>
          <Show when={params.target || params.operation || (params.focus && params.at)}>
            <div class="mb-3 flex flex-wrap items-center gap-2 text-chrome">
              <Show when={params.target}><Chip label="Clear the object filter" onClear={() => setParams({ target: undefined, target_kind: undefined })}>About {targetName()}</Chip></Show>
              <Show when={params.operation}><Chip label="Clear the operation filter" onClear={() => setParams({ operation: undefined, at: undefined })}>One operation</Chip></Show>
              <Show when={params.focus && params.at}><Chip label="Show recent events" onClear={() => setParams({ focus: undefined, at: undefined })}>One linked event</Chip></Show>
            </div>
          </Show>
          <Switch>
            <Match when={isDenied(events.error)}><DeniedState message="You need Read audit events permission for this scope." /></Match>
            <Match when={params.category && actions.data && inCategory().length === 0}><p class={ui.error}>This kind of action is not recorded. <button type="button" class={ui.compactButton} onClick={() => setParams({ category: undefined })}>Clear</button></p></Match>
            <Match when={params.category && actions.isError}><p class={ui.error}>Could not load the kinds of action. <button type="button" class={ui.compactButton} onClick={() => void actions.refetch()}>Retry</button></p></Match>
            <Match when={events.isError && !events.data}><p class={ui.error}>{errorMessage(events.error)}</p></Match>
            <Match when={!events.data}><TableSkeleton label="Loading audit events" columns={COLUMNS} /></Match>
            <Match when={rows().length === 0}><EmptyState title="No events match these filters." hint="Events older than their retention are gone. Organization access changes appear only to people who can read the whole organization's audit events." /></Match>
            <Match when={true}>
              <DataTable columns={COLUMNS} footer={<LoadMore count={rows().length} noun={{ one: "event", other: "events" }} hasMore={events.hasNextPage} loading={events.isFetchingNextPage} failed={events.isFetchNextPageError} onLoadMore={() => void events.fetchNextPage()} />}>
                <For each={rows()}>{(event) => {
                  const actor = actorOf(event);
                  const icon = ACTOR_ICONS[actor.kind];
                  const current = () => params.event === event.id;
                  const summary = () => eventSummary(event, names());
                  return (
                    <>
                      <tr class={cx("cursor-pointer", current() && "bg-console-accent-soft/40 [&>td]:border-b-transparent")} onClick={() => select(current() ? undefined : event)}>
                        <td class="w-6 pr-0!"><button type="button" data-audit-toggle={event.id} class="flex cursor-pointer items-center border-0 bg-transparent p-0 text-console-subtle hover:text-console-text" aria-expanded={current()} aria-controls={current() ? `audit-row-${event.id}` : undefined} aria-label={`${current() ? "Hide" : "Show"} details: ${summary()}`} onClick={(e) => { e.stopPropagation(); select(current() ? undefined : event); }}><Icon name={current() ? "chevron-down" : "chevron-right"} /></button></td>
                        <td class="whitespace-nowrap font-mono text-chrome text-console-muted"><span title={new Date(event.occurred_at).toLocaleString()}><RelativeTime value={event.occurred_at} /></span></td>
                        <td>
                          <span class="inline-flex min-w-0 items-center gap-2">
                            <span class="text-console-subtle">{icon ? <Icon name={icon} /> : <span aria-hidden="true">⚙</span>}</span>
                            <span class="min-w-0 truncate" title={actor.detail}>{actor.name}</span>
                          </span>
                        </td>
                        <td class="max-w-0 w-full truncate text-body text-console-text">{summary()}</td>
                        <td><StatusBadge resource="audit_outcome" status={event.outcome as "succeeded" | "denied"} /></td>
                        <td class="whitespace-nowrap text-chrome text-console-muted">{event.environment_id ? names().get(event.environment_id) ?? "Environment" : "Organization"}</td>
                      </tr>
                      <Show when={current()}>
                        <tr id={`audit-row-${event.id}`} class="hover:bg-transparent">
                          <td colSpan={COLUMNS.length} class="p-0! whitespace-normal">
                            <AuditEventDetails
                              event={event}
                              names={names()}
                              memberHref={memberLink}
                              rolesWith={rolesWith}
                              onSelectOperation={() => { refocus = true; setParams({ ...CLEAR_PREDICATES, operation: event.operation_id, at: event.recorded_at, focus: undefined }); }}
                              onSelectCause={() => { refocus = true; setParams({ ...CLEAR_PREDICATES, event: event.cause_event_id ?? undefined, focus: event.cause_event_id ?? undefined, at: event.recorded_at, operation: undefined }); }}
                              link={link(event)}
                            />
                          </td>
                        </tr>
                      </Show>
                    </>
                  );
                }}</For>
              </DataTable>
            </Match>
          </Switch>
        </Show>
    </div>
  );
}
