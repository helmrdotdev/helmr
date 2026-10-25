import { ApiError } from "../lib/api";
import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, For, Show, type JSX } from "solid-js";
import { SectionLayout } from "../components/SectionLayout";
import { ComputerStatus } from "../features/computers/ComputerStatus";
import { computerHref } from "../features/computers/navigation";
import { listTemplateDefinitions, templateResources } from "../lib/definitions";
import { loadError } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import { listComputers, type ComputerListItem } from "../lib/computers";
import { DataTable, EmptyCell, LoadMore, RowActions } from "../ui/DataTable";
import type { ActionMenuItem } from "../ui/ActionMenu";
import { useCopyLink } from "../components/copyLink";
import { createComputerControls } from "../features/computers/ComputerExecutionControls";
import { IDText } from "../ui/IDText";
import { type LinkTabItem } from "../ui/Tabs";
import { QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";

const COMPUTER_COLUMNS = ["Computer", "Status", "Key", "Template", "Last activity", "Created", { label: "Actions", srOnly: true }] as const;
const TEMPLATE_COLUMNS = ["Template", "Resources"] as const;

const computersErrorMessage = (error: unknown) =>
  loadError(error, "Could not load computers.", { forbidden: "You do not have permission to view computers." });

function ComputerRow(props: { computer: ComputerListItem; menu: ActionMenuItem[] }) {
  return (
    <tr>
      <td><IDText value={props.computer.id} mode="link" href={computerHref(props.computer.id)} /></td>
      <td><ComputerStatus computer={props.computer} /></td>
      <td>
        <Show when={props.computer.key} fallback={<EmptyCell />}>
          {(key) => <code>{key()}</code>}
        </Show>
      </td>
      <td><code>{props.computer.template}</code></td>
      <td><RelativeTime value={props.computer.last_activity_at} /></td>
      <td><RelativeTime value={props.computer.created_at} /></td>
      <RowActions label={`Actions for ${props.computer.id}`} items={props.menu} />
    </tr>
  );
}

type Section = "computers" | "templates";
const SECTION_TABS: LinkTabItem<Section>[] = [
  { value: "computers", label: "Computers", href: "/computers" },
  { value: "templates", label: "Templates", href: "/templates" },
];

// The Computers section: Computers and the Templates that create them.
export function ComputersSection(props: { children?: JSX.Element }) {
  return (
    <SectionLayout id="computers-section" title="Computers" tabs={SECTION_TABS}>
      {props.children}
    </SectionLayout>
  );
}

export function ComputerList() {
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
  const copyLink = useCopyLink();
  const controls = createComputerControls({ computer: () => undefined, scope: () => ({ projectID: projectID(), environmentID: environmentID() }), onChanged: () => computers.refetch() });

  return (
    <>
    <copyLink.LiveRegion />
    <div class="mb-3 flex flex-col gap-2 empty:hidden"><controls.Feedback /></div>
    <QueryState
      query={computers}
      pending={<TableSkeleton label="Loading computers" columns={COMPUTER_COLUMNS} minWidth="min-w-190" />}
      error={computersErrorMessage}
      empty={{
        when: (data) => data.pages.every((page) => page.computers.length === 0),
        title: "No computers yet.",
        hint: "Start an agent through the CLI or Slack, or create a computer with helmr computer create.",
      }}
    >
      {() => (
        <DataTable
          columns={COMPUTER_COLUMNS}
          minWidth="min-w-190"
          footer={<LoadMore count={items().length} noun={{ one: "computer", other: "computers" }} hasMore={computers.hasNextPage} loading={computers.isFetchingNextPage} failed={computers.isFetchNextPageError} onLoadMore={() => void computers.fetchNextPage()} />}
        >
          <For each={items()}>{(computer) => <ComputerRow computer={computer} menu={[...controls.items(computer), copyLink.item(computerHref(computer.id))]} />}</For>
        </DataTable>
      )}
    </QueryState>
    </>
  );
}

const templatesFailure = (error: unknown) => loadError(error, "Could not load templates.", { forbidden: "You do not have permission to view templates." });

// The Templates of the current Deployment, for creating Computers.
export function TemplateList() {
  const scope = useScopeIDs();
  // The API cursor pins subsequent pages to the first page's deployment.
  const definitions = createInfiniteQuery(() => ({
    queryKey: ["templates", "current-definitions", scope().projectID, scope().environmentID],
    queryFn: async ({ pageParam }) => {
      const page = await listTemplateDefinitions({ ...scope(), cursor: pageParam || undefined, limit: 100 }).catch(error => {
        if (error instanceof ApiError && error.code === "no_current_deployment") return { templates: [], deployment_id: "", next_cursor: undefined };
        throw error;
      });
      return { items: page.templates, next_cursor: page.next_cursor, deployment_id: page.deployment_id };
    },
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: !!scope().projectID && !!scope().environmentID, retry: false, refetchInterval: 15_000,
  }));
  const items = createMemo(() => definitions.data?.pages.flatMap(page => page.items) ?? []);
  return (
        <QueryState
          query={definitions}
          pending={<TableSkeleton label="Loading templates" columns={TEMPLATE_COLUMNS} />}
          error={templatesFailure}
          empty={{
            when: (data) => data.pages.every(page => page.items.length === 0),
            title: "No templates in the current deployment.",
            hint: "Add templates in your code and deploy them.",
          }}
        >
          {() => (
            <DataTable
              columns={TEMPLATE_COLUMNS}
              footer={<LoadMore count={items().length} noun={{ one: "template", other: "templates" }} hasMore={definitions.hasNextPage} loading={definitions.isFetchingNextPage} failed={definitions.isFetchNextPageError} onLoadMore={() => void definitions.fetchNextPage()} />}
            >
              <For each={items()}>{item => <tr>
                <td><code>{item.template}</code></td>
                <td>{templateResources(item)}</td>
              </tr>}</For>
            </DataTable>
          )}
        </QueryState>
  );
}
