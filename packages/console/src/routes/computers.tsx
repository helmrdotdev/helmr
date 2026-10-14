import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show, type JSX } from "solid-js";
import { SectionLayout } from "../components/SectionLayout";
import { computerHref } from "../features/computers/navigation";
import { deploymentHref } from "../features/deployments/navigation";
import { listTemplateDefinitions, templateResources } from "../lib/definitions";
import { getCurrentDeployment } from "../lib/deployments";
import { errorMessage } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import { listComputers, type ComputerListItem } from "../lib/computers";
import { DataTable, EmptyCell, LoadMore } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { type LinkTabItem } from "../ui/Tabs";
import { QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, ui } from "../ui/styles";

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
      <td><code>{props.computer.template}</code></td>
      <td><StatusBadge resource="computer" status={props.computer.status} /></td>
      <td><StatusBadge resource="computer_residency" status={props.computer.residency} /></td>
      <td><RelativeTime value={props.computer.last_activity_at} /></td>
      <td><RelativeTime value={props.computer.created_at} /></td>
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

  return (
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
          footer={<LoadMore count={items().length} noun={{ one: "Computer", other: "Computers" }} hasMore={computers.hasNextPage} loading={computers.isFetchingNextPage} failed={computers.isFetchNextPageError} onLoadMore={() => void computers.fetchNextPage()} />}
        >
          <For each={items()}>{(computer) => <ComputerRow computer={computer} />}</For>
        </DataTable>
      )}
    </QueryState>
  );
}

const templatesFailure = (error: unknown) => errorMessage(error, "Could not load Templates.");

// The Templates of the current Deployment, for creating Computers.
export function TemplateList() {
  const scope = useScopeIDs();
  const current = createQuery(() => ({ queryKey: ["deployments", "current", scope().projectID, scope().environmentID], queryFn: () => getCurrentDeployment(scope()), enabled: !!scope().projectID && !!scope().environmentID, retry: false, refetchInterval: 15_000 }));
  // Pin every page to the observed current Deployment. Promotion changes this
  // query key, so pages from different definitions cannot be combined.
  const definitions = createInfiniteQuery(() => ({
    queryKey: ["templates", "current-definitions", scope().projectID, scope().environmentID, current.data?.id],
    queryFn: async ({ pageParam }) => {
      const page = await listTemplateDefinitions({ ...scope(), deploymentID: current.data!.id, cursor: pageParam || undefined, limit: 100 });
      return { items: page.templates, next_cursor: page.next_cursor };
    },
    initialPageParam: "", getNextPageParam: page => page.next_cursor,
    enabled: !!current.data && !current.isError, retry: false,
  }));
  const items = createMemo(() => definitions.data?.pages.flatMap(page => page.items) ?? []);
  return (
    <QueryState
      query={current}
      pending="Loading current Deployment..."
      error={templatesFailure}
      empty={{ when: (deployment) => !deployment, title: "No current Deployment.", hint: "Deploy your code to make its Templates available here." }}
    >
      {deployment => <>
        <p class={cx(ui.muted, "mb-3")}>Templates in the current Deployment <IDText value={deployment()!.id} mode="link" href={deploymentHref(deployment()!.id)} />, for creating Computers.</p>
        <QueryState
          query={definitions}
          pending="Loading Templates..."
          error={templatesFailure}
          empty={{
            when: (data) => data.pages.every(page => page.items.length === 0),
            title: "No Templates in the current Deployment.",
            hint: "Add Templates in your code and deploy them.",
          }}
        >
          {() => (
            <DataTable
              columns={["Template", "Resources"]}
              footer={<LoadMore count={items().length} noun={{ one: "Template", other: "Templates" }} hasMore={definitions.hasNextPage} loading={definitions.isFetchingNextPage} failed={definitions.isFetchNextPageError} onLoadMore={() => void definitions.fetchNextPage()} />}
            >
              <For each={items()}>{item => <tr>
                <td><code>{item.template}</code></td>
                <td>{templateResources(item)}</td>
              </tr>}</For>
            </DataTable>
          )}
        </QueryState>
      </>}
    </QueryState>
  );
}
