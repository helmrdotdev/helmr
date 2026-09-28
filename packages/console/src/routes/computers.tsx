import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { CreateComputerModal } from "../features/computers/CreateComputerModal";
import { computerHref } from "../features/computers/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
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
      <td><span class={ui.muted}>{props.computer.sandbox_id}</span></td>
      <td><StatusBadge resource="computer" status={props.computer.status} /><span class="ml-2">{props.computer.residency}</span></td>
      <td><RelativeTime value={props.computer.last_activity_at} /></td>
      <td><RelativeTime value={props.computer.created_at} /></td>
    </tr>
  );
}

export function Computers() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const [creating, setCreating] = createSignal(false);
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
        subtitle="Durable filesystems created from Sandbox definitions in the selected environment."
        actions={
          <Show when={hasPermission(me.data, "computers.create")}>
            <button type="button" class={ui.button} disabled={!projectID() || !environmentID()} onClick={() => setCreating(true)}>
              Create Computer
            </button>
          </Show>
        }
      />

      <Show when={computers.isError}>
        <StatePanel error={computersErrorMessage(computers.error)} />
      </Show>
      <Show when={!computers.isPending} fallback={<StatePanel loading="Loading Computers..." />}>
        <Show
          when={items().length > 0}
          fallback={<StatePanel empty="No Computers yet." hint="Runs and Sessions create Computers from their Sandbox, or create one here." />}
        >
          <DataTable columns={["Computer", "Key", "Sandbox", "Status", "Last activity", "Created"]} minWidth="min-w-200">
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

      <Show when={creating()}>
        <CreateComputerModal
          projectID={projectID()}
          environmentID={environmentID()}
          onClose={() => setCreating(false)}
          onCreated={async () => {
            await queryClient.invalidateQueries({ queryKey: ["computers"] });
          }}
        />
      </Show>
    </section>
  );
}
