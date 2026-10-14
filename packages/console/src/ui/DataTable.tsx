import { A } from "@solidjs/router";
import { For, Show, type JSX } from "solid-js";
import { ActionMenu, type ActionMenuItem } from "./ActionMenu";
import { cx, ui } from "./styles";

type Column = string | { label: string; srOnly?: boolean };

// The only table. Rows are single-line; secondary information gets its own column.
// `footer` renders under the rows inside the table frame (see LoadMore).
export function DataTable(props: { columns: readonly Column[]; minWidth?: string; footer?: JSX.Element; children: JSX.Element }) {
  return (
    <div class="border border-console-border-strong bg-console-surface">
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

// The table footer: how many rows are shown and, while more exist, "Load more".
export function LoadMore(props: {
  count: number;
  hasMore: boolean;
  loading: boolean;
  onLoadMore: () => void;
  noun?: string;
}) {
  return (
    <div class={ui.tableFooter}>
      <Show when={props.hasMore}>
        <button
          type="button"
          class={cx(ui.secondaryButton, "h-6 min-h-6 px-2")}
          disabled={props.loading}
          onClick={() => props.onLoadMore()}
        >
          {props.loading ? "Loading…" : "Load more"}
        </button>
      </Show>
      <span>
        Showing {props.count}
        {props.noun ? ` ${props.noun}` : ""}
      </span>
    </div>
  );
}
