import { For, type JSX } from "solid-js";
import { cx, ui } from "./styles";

export type Column = string | { label: string; srOnly?: boolean };

export function DataTable(props: { columns: readonly Column[]; minWidth?: string; children: JSX.Element }) {
  return (
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
  );
}
