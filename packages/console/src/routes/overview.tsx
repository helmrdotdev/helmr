import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Match, Show, Switch } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { attentionTotal, useAttention, type AttentionCounts, type AttentionItem, type AttentionKind } from "../lib/attention";
import { agentHref } from "../lib/definitions";
import { getCurrentDeployment, getDeploymentEvents } from "../lib/deployments";
import { errorMessage } from "../lib/error";
import { useScope, useScopeIDs } from "../lib/scope";
import { askConsolePath, listSessions, sessionConsolePath } from "../lib/sessions";
import { CodeBlock } from "../ui/CodeBlock";
import { Segmented } from "../ui/Field";
import { IDText } from "../ui/IDText";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, ErrorState, PendingState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { Steps } from "../ui/Steps";
import { cx, ui } from "../ui/styles";

const overviewError = (error: unknown) => errorMessage(error, "Could not load what is waiting in this environment.");

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
      return item.prompt || "The Agent asked a question.";
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

// The one-line context under the title: the current Deployment and when it was
// promoted, from its latest promotion event.
function DeploymentContext() {
  const scope = useScopeIDs();
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
    enabled: enabled() && !!current.data,
    retry: false,
  }));
  return (
    <Show when={current.isSuccess}>
      <Show when={current.data} fallback={<span>No current Deployment</span>}>
        {(deployment) => (
          <span class="inline-flex flex-wrap items-center gap-1.5">
            Current Deployment
            <IDText value={deployment().id} mode="link" href={deploymentHref(deployment().id)} />
            <Show when={promoted.data}>
              {(at) => <span>· promoted <RelativeTime value={at()} /></span>}
            </Show>
          </span>
        )}
      </Show>
    </Show>
  );
}

// Before the first Deployment: the CLI steps with this tab's Project and
// Environment filled in.
function FirstDeployment(props: { projectID: string; environmentID: string; environmentName: string }) {
  const origin = window.location.origin;
  const scope = () => `--project ${props.projectID} --env ${props.environmentID}`;
  return (
    <section class={ui.window} aria-label={`Set up ${props.environmentName}`}>
      <div class={ui.titleStrip}>
        <h2 class="m-0 min-w-0 flex-1 truncate font-mono text-micro font-normal">Set up {props.environmentName}</h2>
      </div>
      <div class={cx(ui.windowStack, "p-4")}>
        <p class={cx(ui.muted, "m-0")}>Deploy an Agent to this environment. This page lists what waits on you afterwards.</p>
        <Steps
          label="First Deployment"
          steps={[
            {
              title: "Install the CLI and sign in",
              state: "current",
              description: <>
                <span>Signing in opens this console to approve the device.</span>
                <CodeBlock code={`curl -fsSL https://helmr.dev/install | bash\nhelmr login ${origin}`} copy label="Install and sign in" />
              </>,
            },
            {
              title: "Create and deploy an Agent",
              state: "upcoming",
              description: <>
                <span>The starter project defines the hello Agent.</span>
                <CodeBlock code={`helmr init --dir ./my-agent\ncd ./my-agent\nbun install\nhelmr deploy . ${scope()}`} copy label="Create and deploy" />
              </>,
            },
            {
              title: "Start a Session",
              state: "upcoming",
              description: <>
                <span>From the CLI, or from Slack once the Agent is connected.</span>
                <CodeBlock code={`helmr session create hello ${scope()}`} copy label="Start a Session" />
              </>,
            },
          ]}
        />
      </div>
    </section>
  );
}

function QueueItem(props: { item: AttentionItem }) {
  return (
    <li
      class="px-3.5 py-3"
      aria-label={`${KIND_LABELS[props.item.kind]}: ${summary(props.item)}`}
    >
      <div class="flex flex-wrap items-center gap-x-3.5 gap-y-1.5">
        <StatusBadge resource="attention" status={props.item.kind} />
        <span class="min-w-0 flex-[1_1_280px] truncate text-body" title={summary(props.item)}>{summary(props.item)}</span>
        <span class="flex items-center gap-1.5 font-mono text-chrome text-console-muted">
          <A class="text-console-accent hover:text-console-accent-hover" href={agentHref(props.item.agent)}>{props.item.agent}</A>
          <Show when={props.item.session_id}>
            {(session) => <>· <IDText value={session()} mode="link" href={sessionConsolePath(session())} /></>}
          </Show>
        </span>
        <span class="font-mono text-chrome"><RelativeTime value={props.item.at} /></span>
        <A class={cx(ui.secondaryButton, "h-6 min-h-6 px-2")} href={actionHref(props.item)}>{ACTIONS[props.item.kind]}</A>
      </div>
    </li>
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
      <section class={ui.window} aria-label="Waiting on you">
        <div class={ui.titleStrip}>
          <h2 class="m-0 min-w-0 flex-1 truncate font-mono text-micro font-normal">Needs you · oldest first</h2>
        </div>
        <Show when={shown().length > 0} fallback={<p class={cx(ui.muted, "m-0 px-3.5 py-5 text-center")}>Nothing of this kind is waiting.</p>}>
          <ul class="m-0 list-none divide-y divide-console-border-soft p-0" aria-label="Waiting items">
            <For each={shown()}>{(item) => <QueueItem item={item} />}</For>
          </ul>
        </Show>
      </section>
      <For each={truncated()}>
        {(kind) => <p class={cx(ui.muted, "m-0")}>Showing the oldest {props.items.filter((item) => item.kind === kind).length} of {count(kind)} {KIND_LABELS[kind].toLowerCase()} items.</p>}
      </For>
      <p class={cx(ui.muted, "m-0")}>
        Questions leave this list once answered in Slack or the CLI, holds once released, failed Turns once the Session moves on, closes or a day passes, and connections once reauthorized. Session history stays in <A class={ui.monoLink} href="/sessions">Sessions</A>.
      </p>
    </div>
  );
}

export function Overview() {
  const scope = useScope();
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
  // Only an Environment without a current Deployment asks whether it ever had a Session.
  const anySession = createQuery(() => ({
    queryKey: ["sessions", "any", ids().projectID, ids().environmentID],
    queryFn: async () => (await listSessions({ ...ids(), limit: 1 })).sessions.length > 0,
    enabled: enabled() && current.data === null,
    retry: false,
  }));
  const total = () => (attention.data ? attentionTotal(attention.data.counts) : 0);
  const firstRun = () => current.data === null && anySession.data === false && total() === 0;
  // With nothing waiting, wait to tell a new Environment from a quiet one; a
  // failed check shows the quiet state rather than loading forever.
  const deciding = () => total() === 0 && (current.isPending || (current.data === null && anySession.isPending));

  return (
    <Page>
      <PageHeader title="Overview" subtitle="Everything in this environment that is waiting on a person." meta={<DeploymentContext />} />
      <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
        <Switch>
          <Match when={attention.isError && !attention.data}>
            <ErrorState message={overviewError(attention.error)} onRetry={() => void attention.refetch()} />
          </Match>
          <Match when={!attention.data || deciding()}>
            <PendingState>Loading…</PendingState>
          </Match>
          <Match when={firstRun()}>
            <FirstDeployment projectID={ids().projectID} environmentID={ids().environmentID} environmentName={scope.selectedEnvironment()?.name ?? ids().environmentID} />
          </Match>
          <Match when={total() === 0}>
            <EmptyState title="Nothing is waiting on you." hint="Questions, holds, failed Turns and Slack connections that need you appear here as they happen.">
              <A class={ui.monoLink} href="/sessions">View Sessions</A>
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
