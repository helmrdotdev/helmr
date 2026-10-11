import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { computerHref } from "../features/computers/navigation";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import { listComputers, type ComputerListItem } from "../lib/computers";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

function computersErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "forbidden") {
    return "You do not have permission to view Computers.";
  }
  if (error instanceof ApiError) return error.message;
  return "Could not load Computers.";
}

function ComputerRow(props: { computer: ComputerListItem }) {
  return (
    <tr>
      <td><IDText value={props.computer.id} mode="link" href={computerHref(props.computer.id)} /></td>
      <td>
        <Show when={props.computer.key} fallback={<span class="text-console-faint">—</span>}>
          {(key) => <code>{key()}</code>}
        </Show>
      </td>
      <td><span class={ui.muted}>{props.computer.definition_key}</span></td>
      <td><StatusBadge resource="computer" status={props.computer.status} /><span class="ml-2">{props.computer.residency}</span></td>
      <td><RelativeTime value={props.computer.last_activity_at} /></td>
      <td><RelativeTime value={props.computer.created_at} /></td>
    </tr>
  );
}

export function Computers() {
  const scope = useScope();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const computers = createInfiniteQuery(() => ({
    queryKey: ["computers", "list", projectID(), environmentID()],
    queryFn: ({ pageParam }) => listComputers(
      { projectID: projectID(), environmentID: environmentID() },
      { cursor: pageParam || undefined, limit: 100 },
    ),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    enabled: !!projectID() && !!environmentID(),
    retry: false,
  }));
  const items = createMemo(() => computers.data?.pages.flatMap((page) => page.computers) ?? []);

  return (
    <section class={ui.page}>
      <PageHeader
        title="Computers"
        subtitle="Durable Computers created from Computer definitions in the selected environment."
      />

      <Show when={computers.isError}>
        <StatePanel error={computersErrorMessage(computers.error)} />
      </Show>
      <Show when={!computers.isPending} fallback={<StatePanel loading="Loading Computers..." />}>
        <Show
          when={items().length > 0}
          fallback={<StatePanel empty="No Computers yet." hint="Start an Agent through the CLI or Slack, or create a Computer with helmr computer create." />}
        >
          <DataTable columns={["Computer", "Key", "Definition", "Status", "Last activity", "Created"]} minWidth="min-w-200">
            <For each={items()}>
              {(computer) => <ComputerRow computer={computer} />}
            </For>
          </DataTable>
          <Show when={computers.hasNextPage}>
            <div class={ui.actionRow}>
              <button
                type="button"
                class={ui.secondaryButton}
                disabled={computers.isFetchingNextPage}
                onClick={() => void computers.fetchNextPage()}
              >
                {computers.isFetchingNextPage ? "Loading..." : "Load more"}
              </button>
            </div>
          </Show>
        </Show>
      </Show>

    </section>
  );
}
