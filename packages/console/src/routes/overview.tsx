import { hasPermission } from "../lib/auth";
import { useMe } from "../lib/me";
import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Match, Show, Switch, type JSX } from "solid-js";
import { computerHref } from "../features/computers/navigation";
import { deploymentHref } from "../features/deployments/navigation";
import { useAttention } from "../lib/attention";
import { EnvironmentAccessSetup } from "../features/access/EnvironmentAccessSetup";
import { SetUpPanel } from "../features/onboarding/SetUpPanel";
import { UsageBand, useUsage } from "../features/overview/UsageBand";
import { listComputers, type ComputerListItem, type ComputerScope } from "../lib/computers";
import { agentHref, listAgentDefinitions } from "../lib/definitions";
import { getCurrentDeployment, getDeployment, getDeploymentEvents, listDeployments, type Deployment } from "../lib/deployments";
import { failureRate, getOverview, orderComputers, overviewIssues, type Issue, type IssueTarget, type Overview as OverviewData, type OverviewAgent, type OverviewSession } from "../lib/overview";
import { useScopeIDs } from "../lib/scope";
import { formatDuration, type Usage, type UsageRange } from "../lib/usage";
import { askConsolePath, sessionConsolePath } from "../lib/sessions";
import { Icon, type IconName } from "../ui/Icon";
import { IDText } from "../ui/IDText";
import { List, ListItem } from "../ui/List";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { EmptyState, isDenied, NoScopeState } from "../ui/QueryState";
import { LinesSkeleton, PageSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, ui } from "../ui/styles";

// Rows each list shows before pointing to its full page.
const LIST_ROWS = 5;
// The Computers the Overview reads to judge their health, at most.
const COMPUTER_PAGES = 10;
const COMPUTER_PAGE = 100;

// Where an issue's action leads.
function issueHref(target: IssueTarget): string {
  switch (target.kind) {
    case "turn": return `${sessionConsolePath(target.sessionID)}#turn-${target.turnID}`;
    case "session": return sessionConsolePath(target.sessionID);
    case "computers": return "/computers";
    case "slack": return agentHref(target.agent, "slack");
    case "question": return askConsolePath({ sessionID: target.item.session_id ?? "", turnID: target.item.turn_id ?? "", askID: target.item.id });
  }
}

const ISSUE_BAR: Record<Issue["tone"], string> = { danger: "bg-console-danger", warning: "bg-console-warning", neutral: "bg-console-border-strong" };

// What needs someone now, grouped by cause. It sits above everything else and
// only while something is wrong; the rest of the page keeps its place.
function NeedsAttention(props: { issues: Issue[] }) {
  return (
    <Show when={props.issues.length}>
      <section class="border border-console-danger-border bg-console-surface" aria-labelledby="needs-attention">
        <div class={cx(ui.containerHeaderDanger, "justify-between")}>
          <h2 class={cx(ui.containerTitle, "text-console-text")}><span id="needs-attention">Needs attention</span><span class={ui.containerCount}>{props.issues.length}</span></h2>
          <span class="font-mono text-chrome text-console-subtle">Most urgent first</span>
        </div>
        <ul class="m-0 list-none p-0">
          <For each={props.issues}>{(issue) => (
            <li class="grid grid-cols-[3px_minmax(0,1fr)_auto] items-center gap-3 border-b border-console-border py-2 pr-3 last:border-b-0">
              <span class={cx("self-stretch", ISSUE_BAR[issue.tone])} aria-hidden="true" />
              <div class="min-w-0">
                <p class="m-0 truncate text-body font-medium">{issue.title}</p>
                <p class="m-0 mt-0.5 truncate text-chrome text-console-muted">{issue.detail}</p>
              </div>
              <A class={ui.compactButton} href={issueHref(issue.target)}>{issue.action}</A>
            </li>
          )}</For>
        </ul>
      </section>
    </Show>
  );
}

// One of the numbers at the top. A part the caller cannot read shows a dash.
function Tile(props: { label: string; value: JSX.Element; unit: string; note: JSX.Element; href?: string | undefined }) {
  const body = (
    <>
      <span class={ui.microLabel}>{props.label}</span>
      <span class="mt-1.5 flex items-baseline gap-1.5"><span class="text-[1.4rem] font-medium leading-none tabular-nums">{props.value}</span><span class="text-chrome text-console-muted">{props.unit}</span></span>
      <span class="mt-1 min-h-4 truncate text-chrome text-console-muted">{props.note}</span>
    </>
  );
  return (
    <Show when={props.href} fallback={<div class="flex min-w-0 flex-col px-3.5 py-3">{body}</div>}>
      {(href) => <A href={href()} class="flex min-w-0 flex-col px-3.5 py-3 text-console-text no-underline hover:bg-console-bg-panel">{body}</A>}
    </Show>
  );
}

// A read as a section shows it. Denied hides any earlier data; a failed
// refresh keeps the last data and says so.
type Read<T> = { data: T | undefined; denied: boolean; failed: boolean };

function Tiles(props: { overview: Read<OverviewData>; computers: Read<ComputerListItem[]>; computersComplete: boolean; questions: { count: number; oldest: string | undefined } | undefined }) {
  const o = () => props.overview.data;
  const rate = () => { const data = o(); return data ? failureRate(data.turns) : undefined; };
  const before = () => { const data = o(); return data ? failureRate(data.previous) : undefined; };
  // A rate of at least 5% and at least twice the day before's is called out.
  const worse = () => { const r = rate(), b = before(); return r !== undefined && r >= 5 && (b === undefined || r >= b * 2); };
  const unread = (read: Read<unknown>, what: string) => (read.denied ? `No access to ${what}` : read.failed ? (read.data ? `Could not refresh ${what}` : `Could not load ${what}`) : "");
  const computers = () => props.computers.data;
  const waiting = () => computers()?.filter((c) => c.execution_wait).length ?? 0;
  return (
    <div class="grid grid-cols-4 border border-console-border-strong bg-console-surface max-[860px]:grid-cols-2 [&>*]:border-console-border [&>*:not(:first-child)]:border-l max-[860px]:[&>*:nth-child(3)]:border-l-0 max-[860px]:[&>*:nth-child(n+3)]:border-t">
      <Tile label="Running now" href="/sessions" value={o()?.active.running ?? "—"} unit="sessions" note={o() && !props.overview.failed ? `${o()!.active.queued} queued · ${o()!.active.held} held` : unread(props.overview, "sessions")} />
      <Tile label="Turns · 24h" value={o()?.turns.total ?? "—"} unit={o() ? `${o()!.turns.failed} failed` : ""} note={
        <Show when={o() && !props.overview.failed} fallback={unread(props.overview, "sessions")}>
          <span class={cx(worse() && "text-console-danger")}>{rate() === undefined ? "No turns" : `${rate()}% failed`}{before() !== undefined ? ` · ${before()}% the day before` : ""}</span>
        </Show>
      } />
      <Tile label="Computers" href={props.computers.denied ? undefined : "/computers"} value={computers() ? computers()!.filter((c) => c.residency === "running").length : "—"} unit="awake" note={
        <Show when={computers() && !props.computers.failed} fallback={unread(props.computers, "computers")}>
          <span class={cx(waiting() > 0 && "text-console-warning")}>{computers()!.filter((c) => c.residency === "parked").length} sleeping · {waiting() ? `${waiting()} waiting for capacity` : props.computersComplete ? "none waiting" : "none waiting in the first 1,000"}</span>
        </Show>
      } />
      <Tile label="Waiting on people" value={props.questions?.count ?? "—"} unit="questions" note={props.questions?.oldest ? <>Oldest <RelativeTime value={props.questions.oldest} /></> : props.questions ? "None open" : ""} />
    </div>
  );
}

// A titled list on the Overview. Its rows carry their own labels, so there is
// no column header under the title; a read that is loading, denied or failed
// says so in the same place.
function Section<T>(props: { title: string; count?: number | undefined; link?: { href: string; label: string } | undefined; read: Read<T>; what: string; children: (data: T) => JSX.Element }) {
  return (
    <Panel title={props.title} count={props.count} actions={props.link && !props.read.denied ? <A class={ui.monoLink} href={props.link.href}>{props.link.label}</A> : undefined}>
      <Switch>
        <Match when={props.read.denied}><p class={cx(ui.muted, "m-0")}>You do not have access to {props.what} in this environment.</p></Match>
        <Match when={props.read.data}>{(data) => <>
          <Show when={props.read.failed}><p class="m-0 text-chrome text-console-danger">Could not refresh {props.what}. Showing what last loaded.</p></Show>
          {props.children(data())}
        </>}</Match>
        <Match when={props.read.failed}><p class={cx(ui.muted, "m-0 text-console-danger")}>Could not load {props.what}.</p></Match>
        <Match when={true}><LinesSkeleton label={`Loading ${props.what}`} /></Match>
      </Switch>
    </Panel>
  );
}

const time = "w-28 shrink-0 whitespace-nowrap text-right font-mono text-chrome text-console-muted";

// Turns per hour, oldest first; hours with a failure in the danger colour.
function Spark(props: { hourly: number[]; failed: number[] }) {
  const max = () => Math.max(1, ...props.hourly);
  return (
    <span class="inline-flex h-4 shrink-0 items-end gap-px" aria-hidden="true">
      <For each={props.hourly}>{(count, i) => (
        <span class={cx("w-1", (props.failed[i()] ?? 0) > 0 ? "bg-console-danger" : "bg-console-border-strong")} style={{ height: `${Math.max(2, Math.round((count / max()) * 16))}px` }} />
      )}</For>
    </span>
  );
}

// Each Agent's work in the last 24 hours (sessions in progress, turns per hour,
// failures) beside its time and people over the usage window. An Agent that
// worked only earlier in the window still has a row.
function AgentRows(props: { agents: OverviewAgent[]; usage: Usage | undefined }) {
  const empty = (agent: string): OverviewAgent => ({ agent, active: 0, turns: 0, failed: 0, hourly: Array(24).fill(0), hourly_failed: Array(24).fill(0) });
  const sorted = createMemo(() => {
    const today = new Map(props.agents.map((a) => [a.agent, a]));
    for (const a of props.usage?.agents ?? []) if (!today.has(a.agent)) today.set(a.agent, empty(a.agent));
    const seconds = (agent: string) => props.usage?.agents.find((a) => a.agent === agent)?.seconds ?? 0;
    return [...today.values()].sort((a, b) => b.active - a.active || seconds(b.agent) - seconds(a.agent) || b.turns - a.turns || a.agent.localeCompare(b.agent));
  });
  const share = (agent: string) => props.usage?.agents.find((a) => a.agent === agent);
  return (
    <Show when={sorted().length} fallback={<p class={cx(ui.muted, "m-0")}>No agent has worked {props.usage ? `in the last ${props.usage.days} days` : "in the last 24 hours"}.</p>}>
      <p class="m-0 text-chrome text-console-muted">Sessions, turns per hour and failures over the last 24 hours{props.usage ? `; time and people over the last ${props.usage.days} days` : ""}.</p>
      <List label="Agents">
        <For each={sorted()}>{(agent) => (
          <ListItem line label={agent.agent}>
            <A class="min-w-0 flex-1 truncate font-mono text-chrome text-console-accent hover:text-console-accent-hover" href={agentHref(agent.agent)}>{agent.agent}</A>
            <span class="w-20 shrink-0 whitespace-nowrap text-chrome text-console-muted">{agent.active} active</span>
            <Spark hourly={agent.hourly} failed={agent.hourly_failed} />
            <span class="w-16 shrink-0 whitespace-nowrap text-right text-chrome tabular-nums" title={props.usage ? `Agent time over ${props.usage.days} days` : undefined}>{props.usage ? formatDuration(share(agent.agent)?.seconds ?? 0) : "—"}</span>
            <span class="w-20 shrink-0 whitespace-nowrap text-right text-chrome tabular-nums max-sm:hidden" title={props.usage ? `People over ${props.usage.days} days` : undefined}>{props.usage ? `${share(agent.agent)?.people ?? 0} ${share(agent.agent)?.people === 1 ? "person" : "people"}` : "—"}</span>
            <span class={cx("w-24 shrink-0 whitespace-nowrap text-right text-chrome tabular-nums", agent.failed ? "text-console-danger" : "text-console-muted")}>{agent.failed} failed today</span>
          </ListItem>
        )}</For>
      </List>
    </Show>
  );
}

const ORIGINS: Record<OverviewSession["origin"], { label: string; icon?: IconName }> = {
  slack: { label: "Slack channel", icon: "slack-channel" },
  user: { label: "Member", icon: "member" },
  api_key: { label: "API key", icon: "api-key" },
  trigger: { label: "Trigger" },
  session: { label: "Another session" },
};

// Where a Session started; the icon helps scanning, the words say it.
function Origin(props: { session: OverviewSession }) {
  const origin = () => (props.session.slack_channel_id?.startsWith("D") ? { label: "Slack DM", icon: "slack-dm" as IconName } : ORIGINS[props.session.origin]);
  return (
    <span class="inline-flex w-36 shrink-0 items-center gap-1.5 truncate text-chrome max-sm:hidden">
      <Show when={origin().icon}>{(icon) => <span class="text-console-subtle"><Icon name={icon()} /></span>}</Show>
      <span class="truncate">{origin().label}</span>
    </span>
  );
}

function SessionRows(props: { sessions: OverviewSession[] }) {
  return (
    <Show when={props.sessions.length} fallback={<p class={cx(ui.muted, "m-0")}>No session is working right now.</p>}>
      <List label="Sessions with work in progress">
        <For each={props.sessions.slice(0, LIST_ROWS)}>{(session) => (
          <ListItem line>
            <span class="w-20 shrink-0"><StatusBadge resource="session_work" status={session.state} /></span>
            <span class="w-28 shrink-0"><IDText value={session.id} mode="link" href={sessionConsolePath(session.id)} /></span>
            <A class="min-w-0 flex-1 truncate font-mono text-chrome text-console-accent hover:text-console-accent-hover" href={agentHref(session.agent)}>{session.agent}</A>
            <Origin session={session} />
            <span class={time}><RelativeTime value={session.since} /></span>
          </ListItem>
        )}</For>
      </List>
    </Show>
  );
}

const timeWide = "w-44 shrink-0 whitespace-nowrap text-right font-mono text-chrome text-console-muted";

// The newest deployments with the current one marked and the time it was
// promoted. The current one is always listed, first when it is older than the
// rest. Its templates show only while preparing or failed.
function DeploymentRows(props: { deployments: Deployment[]; current: Deployment | null | undefined; promotedAt: string | null | undefined; preparation: Deployment["preparation_state"] }) {
  const rows = createMemo(() => {
    const current = props.current;
    const newest = props.deployments.slice(0, LIST_ROWS);
    return !current || newest.some((d) => d.id === current.id) ? newest : [current, ...newest.slice(0, LIST_ROWS - 1)];
  });
  return (
    <Show when={rows().length} fallback={<p class={cx(ui.muted, "m-0")}>No deployments yet.</p>}>
      <List label="Deployments">
        <For each={rows()}>{(deployment) => {
          const current = () => deployment.id === props.current?.id;
          const templates = () => (current() && (props.preparation === "preparing" || props.preparation === "failed") ? props.preparation : undefined);
          return (
            <ListItem line>
              <span class="w-20 shrink-0"><Show when={current()}><StatusBadge resource="deployment" status="current" /></Show></span>
              <A class="min-w-0 flex-1 truncate font-mono text-chrome text-console-accent hover:text-console-accent-hover" href={deploymentHref(deployment.id)} title={deployment.version}>{deployment.version}</A>
              <Show when={templates()}>{(state) => <span class="shrink-0"><StatusBadge resource="deployment_templates" status={state()} /></span>}</Show>
              <span class={cx(timeWide, !current() && "max-sm:hidden")}>
                <Show when={current() && props.promotedAt} fallback={<RelativeTime value={deployment.created_at} />}>{(at) => <>promoted <RelativeTime value={at()} /></>}</Show>
              </span>
            </ListItem>
          );
        }}</For>
      </List>
    </Show>
  );
}

function ComputerRows(props: { computers: ComputerListItem[]; complete: boolean }) {
  return (
    <Show when={props.computers.length} fallback={<p class={cx(ui.muted, "m-0")}>{props.complete ? "No computers in this environment." : "No computers among the first 1,000 read. Open All computers for the rest."}</p>}>
      <List label="Computers" footer={props.computers.length > LIST_ROWS || !props.complete ? <div class={ui.tableFooter}>Showing {LIST_ROWS} of {props.computers.length}{props.complete ? "" : "+"}</div> : undefined}>
        <For each={props.computers.slice(0, LIST_ROWS)}>{(computer) => (
          <ListItem line>
            <span class="w-40 shrink-0"><Show when={computer.execution_wait} fallback={<StatusBadge resource="computer_residency" status={computer.residency} />}><StatusBadge resource="computer_wait" status="waiting" /></Show></span>
            <span class="min-w-0 flex-1"><IDText value={computer.id} mode="link" href={computerHref(computer.id)} /></span>
            <span class={time}><RelativeTime value={computer.created_at} /></span>
          </ListItem>
        )}</For>
      </List>
    </Show>
  );
}

// Every Computer the Overview judges, up to a cap; deleted ones are left out.
async function readComputers(scope: ComputerScope): Promise<{ computers: ComputerListItem[]; complete: boolean }> {
  const computers: ComputerListItem[] = [];
  let cursor: string | undefined;
  for (let page = 0; page < COMPUTER_PAGES; page++) {
    const result = await listComputers(scope, { cursor, limit: COMPUTER_PAGE });
    computers.push(...result.computers.filter((c) => c.status === "available"));
    cursor = result.next_cursor;
    if (!cursor) return { computers: orderComputers(computers), complete: true };
  }
  return { computers: orderComputers(computers), complete: false };
}

const read = <T,>(query: { data: T | undefined; error: unknown; isError: boolean }, allowed = true): Read<T> => ({
  data: allowed && !isDenied(query.error) ? query.data : undefined,
  denied: !allowed || isDenied(query.error),
  failed: query.isError && !isDenied(query.error),
});

// The Environment for the people who build and run its agents: what needs them,
// the numbers that say whether it is healthy, what is working now, and what
// changed. Every part keeps its place; only Needs attention comes and goes.
export function Overview() {
  const me = useMe();
  const ids = useScopeIDs();
  const enabled = () => !!ids().projectID && !!ids().environmentID;
  const can = (permission: string) => hasPermission(me.data, permission, ids());
  const attention = useAttention();
  const overview = createQuery(() => ({
    queryKey: ["overview", ids().projectID, ids().environmentID],
    queryFn: () => getOverview(ids()),
    enabled: enabled(),
    retry: false,
    refetchInterval: 15_000,
  }));
  const computers = createQuery(() => ({
    queryKey: ["computers", "overview", ids().projectID, ids().environmentID],
    queryFn: () => readComputers(ids()),
    enabled: enabled() && can("computers.read"),
    retry: false,
    refetchInterval: 15_000,
  }));
  // Only people who can read deployments get the setup check and the current
  // Deployment marked in the list.
  const current = createQuery(() => ({
    queryKey: ["deployments", "current", ids().projectID, ids().environmentID],
    queryFn: () => getCurrentDeployment(ids()),
    enabled: enabled() && can("deployments.read"),
    retry: false,
    refetchInterval: 15_000,
  }));
  const deployments = createQuery(() => ({
    queryKey: ["deployments", "recent", ids().projectID, ids().environmentID],
    queryFn: () => listDeployments(ids(), { limit: LIST_ROWS }),
    enabled: enabled() && can("deployments.read"),
    retry: false,
    refetchInterval: 60_000,
  }));
  // The current Deployment's own record says whether its templates are ready.
  const currentRecord = createQuery(() => ({
    queryKey: ["deployments", "record", ids().projectID, ids().environmentID, current.data?.id],
    queryFn: () => getDeployment(current.data!.id, ids()),
    enabled: enabled() && !!current.data && can("deployments.read"),
    retry: false,
    refetchInterval: 15_000,
  }));
  const promoted = createQuery(() => ({
    queryKey: ["deployments", "promoted", ids().projectID, ids().environmentID, current.data?.id],
    queryFn: async () => {
      const page = await getDeploymentEvents(current.data!.id, ids(), { limit: 100 });
      return page.events.filter((event) => event.kind === "deployment.promoted").at(-1)?.at ?? null;
    },
    enabled: enabled() && !!current.data && can("deployments.read"),
    retry: false,
  }));
  // An Environment shows setup until its current Deployment defines an Agent.
  const hasAgents = createQuery(() => ({
    queryKey: ["agents", "any", ids().projectID, ids().environmentID, current.data?.id],
    queryFn: async () => (await listAgentDefinitions({ ...ids(), deploymentID: current.data!.id, limit: 1 })).agents.length > 0,
    enabled: enabled() && !!current.data,
    retry: false,
    // A new Deployment in the same Environment keeps the last answer while it
    // loads, so setup in progress is not replaced by the loading state.
    placeholderData: (previous: boolean | undefined, query?: { queryKey: readonly unknown[] }) =>
      query?.queryKey[2] === ids().projectID && query?.queryKey[3] === ids().environmentID ? previous : undefined,
  }));
  const setUp = () => current.data === null || hasAgents.data === false;
  // Wait to tell a new Environment from a quiet one; a failed check shows the
  // overview rather than loading forever.
  const deciding = () => (current.isPending && can("deployments.read")) || (!!current.data && hasAgents.isPending);
  const issues = createMemo(() => overviewIssues({ now: Date.now(), failures: overview.data?.failures, computers: computers.data?.computers, attention: attention.data?.items }));
  const questions = () => {
    const data = attention.data;
    if (!data || data.counts.question === undefined) return undefined;
    return { count: data.counts.question, oldest: data.items.find((item) => item.kind === "question")?.at };
  };
  const sessions = () => read(overview);
  const machines = () => read(computers, can("computers.read"));
  // The usage band's window also gives the Agents rows their hours and people.
  const [range, setRange] = createSignal<UsageRange>(30);
  const usage = useUsage(range);
  const usageData = () => (isDenied(usage.error) ? undefined : usage.data);
  // The list fails with either of its two reads.
  const deploymentsRead = () => { const list = read(deployments, can("deployments.read")); const mark = read(current, can("deployments.read")); return { ...list, failed: list.failed || mark.failed }; };

  return (
    <Page>
      <PageHeader title="Overview" />
      <Show when={enabled()} fallback={<NoScopeState hint={<>If none are available, ask an access administrator for a role. <A href="/settings/account" class="underline">View your access</A>.</>} />}>
        <Show when={can("assignments.create") && !(can("deployments.publish") && can("deployments.promote"))}>
          <EnvironmentAccessSetup projectID={ids().projectID} environmentID={ids().environmentID} />
        </Show>
        <Switch>
          <Match when={deciding()}>
            <PageSkeleton label="Loading overview" sections={["Agents", "Running now"]} rail="Deployments" />
          </Match>
          <Match when={setUp()}>
            <div class={ui.sectionStack}>
              <NeedsAttention issues={issues()} />
              <Show when={can("deployments.publish") && can("deployments.promote")} fallback={<EmptyState title="No deployed agents are available to you in this environment." hint="Ask a member who can publish and activate deployments to make an agent available." />}>
                <SetUpPanel projectID={ids().projectID} environmentID={ids().environmentID} />
              </Show>
            </div>
          </Match>
          <Match when={true}>
            <div class={ui.sectionStack}>
              <NeedsAttention issues={issues()} />
              <Tiles overview={sessions()} computers={{ ...machines(), data: machines().data?.computers }} computersComplete={computers.data?.complete ?? true} questions={questions()} />
              <UsageBand range={range()} onRange={setRange} usage={usage} />
              <div class="grid grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)] items-start gap-6 max-[1120px]:grid-cols-1">
                <div class={cx(ui.sectionStack, "min-w-0")}>
                  <Section title="Agents" link={{ href: "/agents", label: "All agents" }} read={sessions()} what="sessions">{(o) => <AgentRows agents={o.agents} usage={usageData()} />}</Section>
                  <Section title="Running now" count={overview.data?.active.count} link={{ href: "/sessions", label: "All sessions" }} read={sessions()} what="sessions">{(o) => <SessionRows sessions={o.active.sessions} />}</Section>
                </div>
                <div class={cx(ui.sectionStack, "min-w-0")}>
                  <Section title="Deployments" link={{ href: "/deployments", label: "All deployments" }} read={deploymentsRead()} what="deployments">{(page) => <DeploymentRows deployments={page.deployments} current={current.data} promotedAt={promoted.data} preparation={currentRecord.data?.preparation_state} />}</Section>
                  <Section title="Computers" count={computers.data?.complete ? computers.data.computers.length : undefined} link={{ href: "/computers", label: "All computers" }} read={machines()} what="computers">{(page) => <ComputerRows computers={page.computers} complete={page.complete} />}</Section>
                </div>
              </div>
            </div>
          </Match>
        </Switch>
      </Show>
    </Page>
  );
}
