import { createSignal, For, onCleanup, Show, type JSX } from "solid-js";
import { DataTable, type Column } from "./DataTable";
import { Panel } from "./Panel";
import { cx } from "./styles";

// How long a wait goes unmarked. Most reads finish sooner, and a skeleton that
// flashes for a few frames reads as flicker.
const DELAY_MS = 250;
// A skeleton that replaces one that just left (a page's record, then the list
// it loads next) shows at once, so the wait does not blink out between them.
const HANDOFF_MS = 100;
let lastShownUntil = 0;

// Renders its children only once the wait has lasted DELAY_MS.
export function Delayed(props: { children: JSX.Element }) {
  const [shown, setShown] = createSignal(Date.now() - lastShownUntil < HANDOFF_MS);
  const timer = shown() ? undefined : setTimeout(() => setShown(true), DELAY_MS);
  onCleanup(() => {
    clearTimeout(timer);
    if (shown()) lastShownUntil = Date.now();
  });
  return <Show when={shown()}>{props.children}</Show>;
}

// One static bar standing in for a value. No shimmer: the page stays calm.
export function SkeletonBar(props: { width: string; class?: string }) {
  return <span aria-hidden="true" class={cx("block h-2.5 bg-console-skeleton", props.class)} style={{ width: props.width }} />;
}

// What every skeleton wraps its shape in: announced once as loading, the shape
// itself hidden from assistive technology.
export function Loading(props: { label: string; children: JSX.Element }) {
  return (
    <Delayed>
      <div role="status" aria-busy="true">
        <span class="sr-only">{props.label}</span>
        <div aria-hidden="true">{props.children}</div>
      </div>
    </Delayed>
  );
}

// Bar widths vary row to row so the rows read as content, not as a pattern.
const WIDTHS = ["72%", "54%", "86%", "62%", "44%"];
const width = (row: number, column: number) => WIDTHS[(row + column * 2) % WIDTHS.length]!;
const range = (count: number) => Array.from({ length: count }, (_, index) => index);

function Lines(props: { count: number; column: number }) {
  return (
    <div class="flex flex-col gap-2.5 py-1">
      <For each={range(props.count)}>{(line) => <SkeletonBar width={width(line, props.column)} />}</For>
    </div>
  );
}

// A table whose rows have not arrived: the real column headers over bars.
export function TableSkeleton(props: { label: string; columns: readonly Column[]; minWidth?: string }) {
  return (
    <Loading label={props.label}>
      <DataTable columns={props.columns} minWidth={props.minWidth}>
        <For each={range(5)}>
          {(row) => (
            <tr>
              <For each={props.columns}>
                {(_, column) => <td><SkeletonBar width={width(row, column())} class="max-w-48" /></td>}
              </For>
            </tr>
          )}
        </For>
      </DataTable>
    </Loading>
  );
}

// A container body or list that has not arrived.
export function LinesSkeleton(props: { label: string }) {
  return <Loading label={props.label}><Lines count={3} column={0} /></Loading>;
}

// A page whose resource has not arrived: titled containers with bars,
// optionally beside a rail.
export function PageSkeleton(props: { label: string; sections: readonly string[]; rail?: string }) {
  const sections = (
    <div class="flex min-w-0 flex-col gap-6">
      <For each={props.sections}>{(title) => <Panel title={title}><Lines count={3} column={1} /></Panel>}</For>
    </div>
  );
  return (
    <Loading label={props.label}>
      <Show when={props.rail} fallback={sections}>
        {(rail) => (
          <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-6 max-[960px]:grid-cols-1">
            {sections}
            <Panel title={rail()}><Lines count={4} column={1} /></Panel>
          </div>
        )}
      </Show>
    </Loading>
  );
}

// A small turning ring for an action the person just started and is waiting on
// (a sign-in callback, linking an account). Never for content.
export function Spinner() {
  return (
    <svg aria-hidden="true" class="size-3.5 shrink-0 animate-spin motion-reduce:animate-none" viewBox="0 0 14 14">
      <circle cx="7" cy="7" r="5.5" fill="none" stroke="currentColor" stroke-opacity="0.18" stroke-width="2" />
      <path d="M7 1.5a5.5 5.5 0 0 1 5.5 5.5" fill="none" stroke="var(--console-accent)" stroke-width="2" stroke-linecap="round" />
    </svg>
  );
}
