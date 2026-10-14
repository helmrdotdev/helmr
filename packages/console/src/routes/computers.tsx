import { definitionHref } from "../lib/definitions";
import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { computerHref } from "../features/computers/navigation";
import { errorMessage } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import { listComputers, type ComputerListItem } from "../lib/computers";
import { DataTable, EmptyCell, LoadMore, PrimaryLink } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";

const computersErrorMessage = (error: unknown) =>
  errorMessage(error, "Could not load Computers.", { forbidden: "You do not have permission to view Computers." });

function ComputerRow(props: { computer: ComputerListItem }) {
  return (
    <tr>
      <td><IDText value={props.computer.id} mode="link" href={computerHref(props.computer.id)} /></td>
      <td>
        <Show when={props.computer.key} fallback={<EmptyCell />}>
          {(key) => <code>{key()}</code>}
        </Show>
      </td>
      <td><PrimaryLink href={definitionHref("templates", props.computer.template, props.computer.deployment_id)}>{props.computer.template}</PrimaryLink></td>
      <td><StatusBadge resource="computer" status={props.computer.status} /></td>
      <td>{props.computer.residency}</td>
      <td><RelativeTime value={props.computer.last_activity_at} /></td>
      <td><RelativeTime value={props.computer.created_at} /></td>
    </tr>
  );
}

export function Computers() {
  const scope = useScopeIDs();
  const projectID = () => scope().projectID;
  const environmentID = () => scope().environmentID;
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
    <Page>
      <PageHeader
        title="Computers"
        subtitle="Durable execution environments for Sessions, commands, and files."
      />
      <QueryState
        query={computers}
        pending="Loading Computers..."
        error={computersErrorMessage}
        empty={{
          when: (data) => data.pages.every((page) => page.computers.length === 0),
          title: "No Computers yet.",
          hint: "Start an Agent through the CLI or Slack, or create a Computer with helmr computer create.",
        }}
      >
        {() => (
          <DataTable
            columns={["Computer", "Key", "Template", "Status", "Residency", "Last activity", "Created"]}
            minWidth="min-w-220"
            footer={<LoadMore count={items().length} noun="Computers" hasMore={computers.hasNextPage} loading={computers.isFetchingNextPage} failed={computers.isFetchNextPageError} onLoadMore={() => void computers.fetchNextPage()} />}
          >
            <For each={items()}>{(computer) => <ComputerRow computer={computer} />}</For>
          </DataTable>
        )}
      </QueryState>
    </Page>
  );
}
