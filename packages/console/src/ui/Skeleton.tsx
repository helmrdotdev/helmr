import { createSignal, For, onCleanup, Show, type JSX } from "solid-js";
import { DataTable, type Column } from "./DataTable";
import { Panel } from "./Panel";
import { cx } from "./styles";

// How long a wait goes unmarked. Most reads finish sooner, and a skeleton that
// flashes for a few frames reads as flicker.
export const SKELETON_DELAY_MS = 250;

// Renders its children only once the wait has lasted SKELETON_DELAY_MS.
export function Delayed(props: { children: JSX.Element }) {
  const [shown, setShown] = createSignal(false);
  const timer = setTimeout(() => setShown(true), SKELETON_DELAY_MS);
  onCleanup(() => clearTimeout(timer));
  return <Show when={shown()}>{props.children}</Show>;
}

// One static bar standing in for a value. No shimmer: the page stays calm.
export function SkeletonBar(props: { width: string; class?: string }) {
  return <span aria-hidden="true" class={cx("block h-2.5 bg-console-skeleton", props.class)} style={{ width: props.width }} />;
}

// Bar widths vary row to row so the rows read as content, not as a pattern.
const WIDTHS = ["72%", "54%", "86%", "62%", "44%"];
const width = (row: number, column: number) => WIDTHS[(row + column * 2) % WIDTHS.length]!;

// A table whose rows have not arrived: the real column headers over bars.
export function TableSkeleton(props: { label: string; columns: readonly Column[]; minWidth?: string; rows?: number }) {
  return (
    <Delayed>
      <div aria-busy="true" aria-label={props.label}>
        <DataTable columns={props.columns} minWidth={props.minWidth}>
          <For each={Array.from({ length: props.rows ?? 5 }, (_, row) => row)}>
            {(row) => (
              <tr>
                <For each={props.columns}>
                  {(_, column) => <td><SkeletonBar width={width(row, column())} class="max-w-48" /></td>}
                </For>
              </tr>
            )}
          </For>
        </DataTable>
      </div>
    </Delayed>
  );
}

// Lines of a container body, a list, or a log that has not arrived.
export function LinesSkeleton(props: { label: string; lines?: number; class?: string }) {
  return (
    <Delayed>
      <div class={cx("flex flex-col gap-2.5 py-1", props.class)} aria-busy="true" aria-label={props.label}>
        <For each={Array.from({ length: props.lines ?? 3 }, (_, line) => line)}>
          {(line) => <SkeletonBar width={width(line, 0)} />}
        </For>
      </div>
    </Delayed>
  );
}

// A page whose resource has not arrived: its sections as titled containers
// with bars. Titles are the page's real section names.
export function PageSkeleton(props: { label: string; sections: readonly string[]; rail?: string }) {
  const sections = (
    <div class="flex min-w-0 flex-col gap-6">
      <For each={props.sections}>
        {(title) => <Panel title={title}><SkeletonLines lines={3} /></Panel>}
      </For>
    </div>
  );
  return (
    <Delayed>
      <div aria-busy="true" aria-label={props.label}>
        <Show when={props.rail} fallback={sections}>
          {(rail) => (
            <div class="grid grid-cols-[minmax(0,1fr)_300px] items-start gap-6 max-[960px]:grid-cols-1">
              {sections}
              <Panel title={rail()}><SkeletonLines lines={4} /></Panel>
            </div>
          )}
        </Show>
      </div>
    </Delayed>
  );
}

function SkeletonLines(props: { lines: number }) {
  return (
    <div class="flex flex-col gap-2.5 py-1">
      <For each={Array.from({ length: props.lines }, (_, line) => line)}>{(line) => <SkeletonBar width={width(line, 1)} />}</For>
    </div>
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
