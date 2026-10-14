import { A } from "@solidjs/router";
import { For, Show, type JSX } from "solid-js";
import { ActionMenu, type ActionMenuItem } from "./ActionMenu";
import { cx, ui } from "./styles";

type Column = string | { label: string; srOnly?: boolean };

// The only table. Rows are single-line; secondary information gets its own column.
// `footer` renders under the rows inside the table frame (see LoadMore). As a
// direct child of a Panel body it has no frame and runs to the Panel's edges.
export function DataTable(props: { columns: readonly Column[]; minWidth?: string; footer?: JSX.Element; children: JSX.Element }) {
  return (
    <div class={ui.container} data-list-frame>
      <div class={ui.tableWrap}>
        <table class={cx(props.minWidth ?? "min-w-160")}>
          <thead>
            <tr>
              <For each={props.columns}>
                {(column) => (
                  <th>
                    {typeof column === "string" ? column : column.srOnly ? <span class="sr-only">{column.label}</span> : column.label}
                  </th>
                )}
              </For>
            </tr>
          </thead>
          <tbody>{props.children}</tbody>
        </table>
      </div>
      {props.footer}
    </div>
  );
}

// The primary cell: the link to the row's resource. Names and keys render mono.
export function PrimaryLink(props: { href: string; children: JSX.Element; title?: string }) {
  return (
    <A href={props.href} class={ui.monoLink} title={props.title}>
      {props.children}
    </A>
  );
}

// A cell with no value.
export function EmptyCell() {
  return <span class={ui.emptyCell}>—</span>;
}

// The trailing actions cell: one row menu, or nothing when no action applies.
export function RowActions(props: { label: string; items: ActionMenuItem[] }) {
  return (
    <td class={ui.actionsCell}>
      <Show when={props.items.length > 0}>
        <ActionMenu label={props.label} items={props.items} />
      </Show>
    </td>
  );
}

// An action's error, as its own row directly under the row it belongs to.
export function RowError(props: { columns: number; message: string | null | undefined }) {
  return (
    <Show when={props.message}>
      <tr class="bg-console-danger-bg/40">
        <td colSpan={props.columns} class="text-control! text-console-danger!" role="alert">{props.message}</td>
      </tr>
    </Show>
  );
}

// What a footer counts: its singular and plural noun, lower case like every
// noun mid-sentence ("session", "sessions").
export type Noun = { one: string; other: string };

// The footer of a table or list: how many rows are shown ("Showing 1 turn",
// "Showing 3 turns") and, while more exist, "Load more", named for what it loads
// ("Load more sessions") so several lists on one page stay distinguishable. When
// loading the next page failed, the footer says so and the button retries just
// that page.
export function LoadMore(props: {
  count: number;
  hasMore: boolean;
  loading: boolean;
  failed?: boolean | undefined;
  onLoadMore: () => void;
  noun: Noun;
  // A second kind of row that the table also shows and that "Load more" loads
  // (the Members table: members, then invitations). It joins the count only
  // when there are some: "Showing 3 members, 1 invitation".
  extra?: { count: number; noun: Noun };
}) {
  const more = () => props.extra?.noun.other ?? props.noun.other;
  const counted = (count: number, noun: Noun) => `${count} ${count === 1 ? noun.one : noun.other}`;
  return (
    <div class={ui.tableFooter}>
      <Show when={props.hasMore}>
        <button
          type="button"
          class={ui.compactButton}
          disabled={props.loading}
          aria-label={props.failed ? `Retry loading more ${more()}` : `Load more ${more()}`}
          onClick={() => props.onLoadMore()}
        >
          {props.loading ? "Loading…" : props.failed ? "Retry" : "Load more"}
        </button>
      </Show>
      <span>
        Showing {counted(props.count, props.noun)}
        <Show when={props.extra && props.extra.count > 0}>, {counted(props.extra!.count, props.extra!.noun)}</Show>
      </span>
      <Show when={props.failed && !props.loading}>
        <span class="text-console-danger" role="alert">Could not load more {more()}.</span>
      </Show>
    </div>
  );
}
