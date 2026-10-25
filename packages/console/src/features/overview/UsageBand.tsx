import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createSignal, For, Match, Show, Switch, type JSX } from "solid-js";
import { agentHref } from "../../lib/definitions";
import { useScopeIDs } from "../../lib/scope";
import { sessionConsolePath } from "../../lib/sessions";
import { bucketLabels, change, channelLabel, formatDuration, getUsage, type Usage, type UsageRange } from "../../lib/usage";
import { Segmented } from "../../ui/Field";
import { Panel } from "../../ui/Panel";
import { isDenied } from "../../ui/QueryState";
import { LinesSkeleton } from "../../ui/Skeleton";
import { cx, ui } from "../../ui/styles";

const RANGES: { value: `${UsageRange}`; label: string }[] = [
  { value: "7", label: "7 days" },
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
];

const CHART_HEIGHT = 96;

// The day a daily slice stands for, counted back from today.
function dayLabel(index: number, days: number): string {
  const back = days - 1 - index;
  if (back === 0) return "Today";
  const date = new Date(Date.now() - back * 86_400_000);
  return date.toLocaleDateString("en-US", { month: "short", day: "numeric" });
}

// One chart's frame: its label, headline, the note under it, a legend for the
// single series, and the plot.
function Chart(props: { label: string; value: string; unit: string; note: JSX.Element; legend: string; color: string; children: JSX.Element; footer?: JSX.Element }) {
  return (
    <div class="flex min-w-0 flex-col px-3.5 py-3">
      <span class={ui.microLabel}>{props.label}</span>
      <span class="mt-1.5 flex items-baseline gap-1.5"><span class="text-[1.25rem] font-medium leading-none tabular-nums">{props.value}</span><span class="text-chrome text-console-muted">{props.unit}</span></span>
      <span class="mt-1 min-h-4 text-chrome text-console-muted">{props.note}</span>
      <span class="mt-2 inline-flex items-center gap-1.5 text-chrome text-console-muted"><span class="size-2 rounded-[2px]" style={{ background: props.color }} aria-hidden="true" />{props.legend}</span>
      {props.children}
      {props.footer}
    </div>
  );
}

// Arrow keys move the inspected day; Home and End jump to the ends.
function inspectDay(event: KeyboardEvent, length: number, hover: () => number | undefined, setHover: (index: number) => void) {
  const current = hover() ?? length - 1;
  const next = event.key === "ArrowLeft" ? current - 1 : event.key === "ArrowRight" ? current + 1 : event.key === "Home" ? 0 : event.key === "End" ? length - 1 : undefined;
  if (next === undefined) return;
  event.preventDefault();
  setHover(Math.min(length - 1, Math.max(0, next)));
}

// Daily values as bars, with the inspected day's value above the plot.
function DailyBars(props: { values: number[]; days: number; color: string; format: (value: number) => string; label: string }) {
  const [hover, setHover] = createSignal<number>();
  const max = () => Math.max(1, ...props.values);
  const width = 330;
  const step = () => width / props.values.length;
  return (
    <div class="relative mt-2">
      <div class="h-4 text-chrome text-console-text tabular-nums" aria-live="polite">{hover() !== undefined ? `${dayLabel(hover()!, props.days)} · ${props.format(props.values[hover()!] ?? 0)}` : ""}</div>
      <svg viewBox={`0 0 ${width} ${CHART_HEIGHT + 14}`} class="block w-full outline-none focus-visible:ring-2 focus-visible:ring-console-accent" role="img" aria-label={`${props.label}. Use the arrow keys to read each day.`} tabIndex={0}
        onMouseLeave={() => setHover(undefined)} onBlur={() => setHover(undefined)} onKeyDown={(event) => inspectDay(event, props.values.length, hover, setHover)}>
        <line x1="0" x2={width} y1={CHART_HEIGHT} y2={CHART_HEIGHT} stroke="var(--console-border-strong)" />
        <For each={props.values}>{(value, i) => {
          const height = () => (value / max()) * (CHART_HEIGHT - 6);
          return (
            <g onMouseEnter={() => setHover(i())}>
              <rect x={i() * step()} y="0" width={step()} height={CHART_HEIGHT} fill="transparent" />
              <rect x={i() * step() + 0.75} y={CHART_HEIGHT - height()} width={Math.max(1, step() - 1.5)} height={Math.max(value > 0 ? 1 : 0, height())} rx="1.2" fill={props.color} opacity={hover() === undefined || hover() === i() ? 1 : 0.5} />
            </g>
          );
        }}</For>
        <text x="0" y={CHART_HEIGHT + 12} font-size="10" fill="var(--console-subtle)">{dayLabel(0, props.days)}</text>
        <text x={width} y={CHART_HEIGHT + 12} font-size="10" fill="var(--console-subtle)" text-anchor="end">Today</text>
      </svg>
    </div>
  );
}

// A daily series as a filled line, with the inspected day's value above it.
function DailyLine(props: { values: number[]; days: number; color: string; format: (value: number) => string; label: string }) {
  const [hover, setHover] = createSignal<number>();
  const width = 330;
  const max = () => Math.max(1, ...props.values) * 1.1;
  const x = (i: number) => (props.values.length < 2 ? 0 : (i * width) / (props.values.length - 1));
  const y = (v: number) => CHART_HEIGHT - (v / max()) * (CHART_HEIGHT - 6);
  const points = () => props.values.map((v, i) => `${x(i)},${y(v)}`).join(" ");
  return (
    <div class="relative mt-2">
      <div class="h-4 text-chrome text-console-text tabular-nums" aria-live="polite">{hover() !== undefined ? `${dayLabel(hover()!, props.days)} · ${props.format(props.values[hover()!] ?? 0)}` : ""}</div>
      <svg viewBox={`0 0 ${width} ${CHART_HEIGHT + 14}`} class="block w-full outline-none focus-visible:ring-2 focus-visible:ring-console-accent" role="img" aria-label={`${props.label}. Use the arrow keys to read each day.`} tabIndex={0}
        onBlur={() => setHover(undefined)} onKeyDown={(event) => inspectDay(event, props.values.length, hover, setHover)}
        onMouseMove={(event) => { const box = event.currentTarget.getBoundingClientRect(); setHover(Math.min(props.values.length - 1, Math.max(0, Math.round(((event.clientX - box.left) / box.width) * (props.values.length - 1))))); }}
        onMouseLeave={() => setHover(undefined)}>
        <line x1="0" x2={width} y1={CHART_HEIGHT} y2={CHART_HEIGHT} stroke="var(--console-border-strong)" />
        <polygon points={`0,${CHART_HEIGHT} ${points()} ${width},${CHART_HEIGHT}`} fill={props.color} opacity="0.12" />
        <polyline points={points()} fill="none" stroke={props.color} stroke-width="2" stroke-linejoin="round" />
        <Show when={hover() !== undefined}>
          <line x1={x(hover()!)} x2={x(hover()!)} y1="0" y2={CHART_HEIGHT} stroke="var(--console-border-strong)" />
          <circle cx={x(hover()!)} cy={y(props.values[hover()!] ?? 0)} r="4" fill={props.color} stroke="var(--console-surface)" stroke-width="2" />
        </Show>
        <text x="0" y={CHART_HEIGHT + 12} font-size="10" fill="var(--console-subtle)">{dayLabel(0, props.days)}</text>
        <text x={width} y={CHART_HEIGHT + 12} font-size="10" fill="var(--console-subtle)" text-anchor="end">Today</text>
      </svg>
    </div>
  );
}

// Turn counts by how long they ran, each bar labelled with its count.
function RunBars(props: { buckets: number[]; labels: string[]; color: string }) {
  const width = 330;
  const gap = 8;
  const max = () => Math.max(1, ...props.buckets);
  const bar = () => (width - gap * (props.buckets.length - 1)) / props.buckets.length;
  return (
    <svg viewBox={`0 0 ${width} ${CHART_HEIGHT + 14}`} class="mt-6 block w-full" role="img" aria-label={`Turns by length: ${props.labels.map((l, i) => `${l} ${props.buckets[i]}`).join(", ")}`}>
      <line x1="0" x2={width} y1={CHART_HEIGHT} y2={CHART_HEIGHT} stroke="var(--console-border-strong)" />
      <For each={props.buckets}>{(count, i) => {
        const height = () => (count / max()) * (CHART_HEIGHT - 20);
        const left = () => i() * (bar() + gap);
        return (
          <g>
            <title>{`${props.labels[i()]}: ${count} turns`}</title>
            <rect x={left()} y={CHART_HEIGHT - height()} width={bar()} height={Math.max(count ? 1 : 0, height())} rx="2" fill={props.color} />
            <text x={left() + bar() / 2} y={CHART_HEIGHT - height() - 4} font-size="10" text-anchor="middle" fill="var(--console-muted)">{count.toLocaleString("en-US")}</text>
            <text x={left() + bar() / 2} y={CHART_HEIGHT + 12} font-size="10" text-anchor="middle" fill="var(--console-subtle)">{props.labels[i()]}</text>
          </g>
        );
      }}</For>
    </svg>
  );
}

function Charts(props: { usage: Usage }) {
  const u = () => props.usage;
  // The share needs the headcount, which only people who can read members get.
  const share = () => { const members = u().people.members; return members ? `${Math.round((u().people.weekly / members) * 100)}% of members · ` : ""; };
  const unassisted = () => (u().runs.completed ? Math.round((u().runs.unassisted / u().runs.completed) * 100) : undefined);
  const longRuns = () => u().runs.buckets.slice(3).reduce((sum, n) => sum + n, 0);
  const vs = () => change(u().work.seconds, u().work.previous_seconds);
  return (
    <div data-list-frame class="grid grid-cols-3 border-y max-[960px]:grid-cols-1 [&>*]:border-console-border [&>*:not(:first-child)]:border-l max-[960px]:[&>*:not(:first-child)]:border-l-0 max-[960px]:[&>*:not(:first-child)]:border-t">
      <Chart label="Agent work" value={formatDuration(u().work.seconds)} unit="of agent time" color="var(--console-accent)" legend="Time spent on turns per day"
        note={<>{vs() ? `${vs()} vs the ${u().days} days before · ` : ""}{u().work.turns.toLocaleString("en-US")} turns</>}>
        <DailyBars values={u().work.daily} days={u().days} color="var(--console-accent)" format={formatDuration} label={`Agent time per day over ${u().days} days`} />
      </Chart>
      <Chart label="People" value={`${u().people.weekly}`} unit="people this week" color="var(--console-info)" legend="People active in the 7 days to each day"
        note={`${share()}${u().people.slack} via Slack, ${u().people.app} only via the CLI, Console or API`}
        footer={<Show when={u().people.channels.length}>
          <ul class="m-0 mt-3 flex list-none flex-col gap-1 p-0 text-chrome">
            <For each={u().people.channels}>{(channel) => <li class="flex"><span class="min-w-0 flex-1 truncate font-mono">{channelLabel(channel)}</span><span class="text-console-muted">{channel.people} {channel.people === 1 ? "person" : "people"}</span></li>}</For>
          </ul>
        </Show>}>
        <DailyLine values={u().people.daily} days={u().days} color="var(--console-info)" format={(n) => `${n} ${n === 1 ? "person" : "people"}`} label={`Weekly active people over ${u().days} days`} />
      </Chart>
      <Chart label="Long runs" value={formatDuration(u().runs.median_seconds)} unit="median turn" color="var(--console-accent)" legend="Turns by how long they ran"
        note={<>{longRuns()} {longRuns() === 1 ? "turn" : "turns"} ran over an hour{unassisted() !== undefined ? ` · ${unassisted()}% finished without asking a person` : ""}</>}
        footer={<Show when={u().runs.longest}>{(longest) => (
          <p class="m-0 mt-3 text-chrome text-console-muted">Longest: <A class="font-mono text-console-accent hover:text-console-accent-hover" href={agentHref(longest().agent)}>{longest().agent}</A> · <A class="text-console-accent hover:text-console-accent-hover" href={`${sessionConsolePath(longest().session_id)}#turn-${longest().turn_id}`}>{formatDuration(longest().seconds)}</A></p>
        )}</Show>}>
        <RunBars buckets={u().runs.buckets} labels={bucketLabels(u().runs.bounds)} color="var(--console-accent)" />
      </Chart>
    </div>
  );
}

// How much the organization uses this Environment's agents: time agents
// worked, people using them and how long they ran on their own. Each count is
// of Sessions the viewer can read; nobody is ranked. The Overview owns the
// window and the read, so its Agents rows show the same window.
export function UsageBand(props: { range: UsageRange; onRange: (range: UsageRange) => void; usage: { data: Usage | undefined; error: unknown; isError: boolean } }) {
  return (
    <Panel title="Agent usage" actions={<Segmented label="Usage range" value={`${props.range}` as `${UsageRange}`} options={RANGES} onChange={(value) => props.onRange(Number(value) as UsageRange)} />}>
      <Switch>
        <Match when={isDenied(props.usage.error)}><p class={cx(ui.muted, "m-0")}>You do not have access to sessions in this environment.</p></Match>
        <Match when={props.usage.data}>{(data) => <>
          <Show when={props.usage.isError}><p class="m-0 text-chrome text-console-danger">Could not refresh usage. Showing what last loaded.</p></Show>
          <Charts usage={data()} />
        </>}</Match>
        <Match when={props.usage.isError}><p class={cx(ui.muted, "m-0 text-console-danger")}>Could not load usage.</p></Match>
        <Match when={true}><LinesSkeleton label="Loading usage" /></Match>
      </Switch>
    </Panel>
  );
}

// The Overview's usage read for the selected window, kept while a new window
// loads in the same Environment.
export function useUsage(range: () => UsageRange) {
  const ids = useScopeIDs();
  return createQuery(() => ({
    queryKey: ["usage", ids().projectID, ids().environmentID, range()],
    queryFn: () => getUsage(ids(), range()),
    enabled: !!ids().projectID && !!ids().environmentID,
    retry: false,
    refetchInterval: 300_000,
    placeholderData: (previous: Usage | undefined, query?: { queryKey: readonly unknown[] }) =>
      query?.queryKey[1] === ids().projectID && query?.queryKey[2] === ids().environmentID ? previous : undefined,
  }));
}
