import { hasPermission } from "../lib/auth";
import { useMe } from "../lib/me";
import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Match, Show, Switch } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { attentionTotal, useAttention, type AttentionCounts, type AttentionItem, type AttentionKind } from "../lib/attention";
import { SetUpPanel } from "../features/onboarding/SetUpPanel";
import { agentHref, listAgentDefinitions } from "../lib/definitions";
import { getCurrentDeployment, getDeploymentEvents } from "../lib/deployments";
import { errorDetails, loadError } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import { askConsolePath, sessionConsolePath } from "../lib/sessions";
import { Segmented } from "../ui/Field";
import { IDText } from "../ui/IDText";
import { List, ListItem } from "../ui/List";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { EmptyState, ErrorState } from "../ui/QueryState";
import { PageSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, ui } from "../ui/styles";

const overviewError = (error: unknown) => loadError(error, "Could not load what is waiting in this environment.", {forbidden:"You do not have run or Agent connection access in this environment. View your access in My account or ask an access administrator for a Role."});

type Filter = "all" | AttentionKind;

const KIND_LABELS: Record<AttentionKind, string> = {
  question: "Questions",
  hold: "Held",
  failed_turn: "Failed",
  slack_reauthorization: "Connections",
};

const ACTIONS: Record<AttentionKind, string> = {
  question: "Answer",
  hold: "Review",
  failed_turn: "Inspect",
  slack_reauthorization: "Reauthorize",
};

// What one waiting item is about, on one line.
function summary(item: AttentionItem): string {
  switch (item.kind) {
    case "question":
      return item.prompt || "The agent asked a question.";
    case "hold":
      return item.reason || "Session held.";
    case "failed_turn":
      return `Turn #${item.turn_sequence ?? "?"} failed${item.error_code ? `: ${item.error_code}` : ""}`;
    case "slack_reauthorization":
      return `Slack connection lost authorization${item.workspace_name ? ` in ${item.workspace_name}` : ""}`;
  }
}

// Where the item's primary action leads.
function actionHref(item: AttentionItem): string {
  switch (item.kind) {
    case "question":
      return askConsolePath({ sessionID: item.session_id ?? "", turnID: item.turn_id ?? "", askID: item.id });
    case "hold":
      return sessionConsolePath(item.session_id ?? "");
    case "failed_turn":
      return `${sessionConsolePath(item.session_id ?? "")}#turn-${item.turn_id ?? item.id}`;
    case "slack_reauthorization":
      return agentHref(item.agent, "slack");
  }
}

// The metadata under the title: the current Deployment and when it was
// promoted, from its latest promotion event, as separate meta items.
function DeploymentContext() {
  const scope = useScopeIDs();
  const me = useMe();
  const canRead = () => hasPermission(me.data, "deployments.read", scope());
  const enabled = () => !!scope().projectID && !!scope().environmentID;
  const current = createQuery(() => ({
    queryKey: ["deployments", "current", scope().projectID, scope().environmentID],
    queryFn: () => getCurrentDeployment(scope()),
    enabled: enabled(),
    retry: false,
    refetchInterval: 15_000,
  }));
  const promoted = createQuery(() => ({
    queryKey: ["deployments", "promoted", scope().projectID, scope().environmentID, current.data?.id],
    queryFn: async () => {
      const page = await getDeploymentEvents(current.data!.id, scope(), { limit: 100 });
      return page.events.filter((event) => event.kind === "deployment.promoted").at(-1)?.at ?? null;
    },
    enabled: enabled() && !!current.data && canRead(),
    retry: false,
  }));
  return (
    <Show when={current.isSuccess}>
      <Show when={current.data} fallback={<span>No current deployment</span>}>
        {(deployment) => (
          <>
            <span class="whitespace-nowrap">Current deployment <Show when={canRead()} fallback={<IDText value={deployment().id} />}><IDText value={deployment().id} mode="link" href={deploymentHref(deployment().id)} /></Show></span>
            <Show when={promoted.data}>
              {(at) => <span class="whitespace-nowrap">Promoted <RelativeTime value={at()} /></span>}
            </Show>
          </>
        )}
      </Show>
    </Show>
  );
}

function QueueItem(props: { item: AttentionItem }) {
  return (
    <ListItem line class="max-sm:flex-wrap max-sm:gap-y-1.5 max-sm:py-2" label={`${KIND_LABELS[props.item.kind]}: ${summary(props.item)}`}>
        <StatusBadge resource="attention" status={props.item.kind} />
        <span class="min-w-0 flex-1 truncate text-body max-sm:basis-full max-sm:order-first max-sm:line-clamp-2 max-sm:whitespace-normal max-sm:wrap-anywhere" title={summary(props.item)}>{summary(props.item)}</span>
        <span class="flex shrink-0 items-center gap-1.5 font-mono text-chrome text-console-muted">
          <A class="text-console-accent hover:text-console-accent-hover" href={agentHref(props.item.agent)}>{props.item.agent}</A>
          <Show when={props.item.session_id}>
            {(session) => <>· <IDText value={session()} mode="link" href={sessionConsolePath(session())} /></>}
          </Show>
        </span>
        <span class="shrink-0 font-mono text-chrome"><RelativeTime value={props.item.at} /></span>
        <A class={cx(ui.compactButton, "ml-auto")} href={actionHref(props.item)}>{ACTIONS[props.item.kind]}</A>
    </ListItem>
  );
}

function Queue(props: { items: readonly AttentionItem[]; counts: AttentionCounts }) {
  const [filter, setFilter] = createSignal<Filter>("all");
  const kinds = (): AttentionKind[] => [
    "question", "hold", "failed_turn",
    ...(props.counts.slack_reauthorization === undefined ? [] : ["slack_reauthorization" as const]),
  ];
  const count = (kind: AttentionKind) => props.counts[kind] ?? 0;
  const options = () => [
    { value: "all" as Filter, label: "All", count: attentionTotal(props.counts) },
    ...kinds().map((kind) => ({ value: kind as Filter, label: KIND_LABELS[kind], count: count(kind) })),
  ];
  const shown = createMemo(() => filter() === "all" ? props.items : props.items.filter((item) => item.kind === filter()));
  // Each kind returns its oldest 100; say so where more are waiting.
  const truncated = createMemo(() => kinds().filter((kind) => (filter() === "all" || filter() === kind) && count(kind) > props.items.filter((item) => item.kind === kind).length));
  return (
    <div class="flex flex-col gap-3">
      <div class="flex flex-wrap items-center gap-2">
        <Segmented label="Filter by kind" value={filter()} options={options()} onChange={setFilter} />
      </div>
      <Panel title="Waiting on you" count={shown().length} actions={<span class="font-mono text-chrome text-console-muted">Oldest first</span>}>
        <Show when={shown().length > 0} fallback={<p class={cx(ui.muted, "m-0 py-2 text-center")}>Nothing of this kind is waiting.</p>}>
          <List label="Waiting items">
            <For each={shown()}>{(item) => <QueueItem item={item} />}</For>
          </List>
        </Show>
      </Panel>
      <For each={truncated()}>
        {(kind) => <p class={cx(ui.muted, "m-0")}>Showing the oldest {props.items.filter((item) => item.kind === kind).length} of {count(kind)} {KIND_LABELS[kind].toLowerCase()} items.</p>}
      </For>
    </div>
  );
}

export function Overview() {
  const me = useMe();
  const ids = useScopeIDs();
  const enabled = () => !!ids().projectID && !!ids().environmentID;
  const attention = useAttention();
  const current = createQuery(() => ({
    queryKey: ["deployments", "current", ids().projectID, ids().environmentID],
    queryFn: () => getCurrentDeployment(ids()),
    enabled: enabled(),
    retry: false,
    refetchInterval: 15_000,
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
  const total = () => (attention.data ? attentionTotal(attention.data.counts) : 0);
  const setUp = () => current.data === null || hasAgents.data === false;
  // Wait to tell a new Environment from a quiet one; a failed check shows the
  // queue rather than loading forever.
  const deciding = () => current.isPending || (!!current.data && hasAgents.isPending);

  return (
    <Page>
      <PageHeader title="Overview" meta={<DeploymentContext />} />
      <Show when={enabled()} fallback={<EmptyState title="No environment selected" hint={<>Select an accessible project and environment. If none are available, ask an access administrator to assign a Role. <A href="/settings/account" class="underline">View your access</A>.</>} />}>
        <Switch>
          <Match when={attention.isError && !attention.data}>
            <ErrorState message={overviewError(attention.error)} details={errorDetails(attention.error)} onRetry={() => void attention.refetch()} />
          </Match>
          <Match when={!attention.data || deciding()}>
            <PageSkeleton label="Loading overview" sections={["Waiting on you"]} />
          </Match>
          <Match when={setUp()}>
            <div class={ui.sectionStack}>
              <Show when={hasPermission(me.data, "deployments.publish", ids()) && hasPermission(me.data, "deployments.promote", ids())} fallback={<EmptyState title="No deployed agents are available to you in this environment." hint="Ask a member who can publish and activate deployments to make an Agent available." />}><SetUpPanel projectID={ids().projectID} environmentID={ids().environmentID} /></Show>
              <Show when={attention.data && total() > 0}>
                <Queue items={attention.data!.items} counts={attention.data!.counts} />
              </Show>
            </div>
          </Match>
          <Match when={total() === 0}>
            <EmptyState title="Nothing is waiting on you." hint="Questions, holds, failed turns and Slack connections that need you appear here as they happen.">
              <A class={ui.monoLink} href="/sessions">View sessions</A>
            </EmptyState>
          </Match>
          <Match when={attention.data}>
            {(data) => <Queue items={data().items} counts={data().counts} />}
          </Match>
        </Switch>
      </Show>
    </Page>
  );
}
