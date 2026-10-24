import { useMe } from "../lib/me";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { deploymentHref, shortVersion } from "../features/deployments/navigation";
import { canPromoteDeployment, PromoteDeploymentModal } from "../features/deployments/PromoteDeploymentModal";
import { defaultEnvironmentColor } from "../features/projects/display";
import { loadError } from "../lib/error";
import { hasPermission } from "../lib/auth";
import { getCurrentDeployment, listDeployments, type Deployment } from "../lib/deployments";
import { useScope } from "../lib/scope";
import { DataTable, EmptyCell, LoadMore, PrimaryLink, RowActions } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { cx } from "../ui/styles";

const COLUMNS = ["Version", "Status", "Digest", "Created", { label: "Actions", srOnly: true }] as const;

const deploymentsErrorMessage = (error: unknown) =>
  loadError(error, "Could not load deployments.", { forbidden: "You do not have permission to view deployments." });

function DeploymentRow(props: {
  deployment: Deployment;
  current: boolean;
  environmentColor: string;
  canPromote: boolean;
  onPromote: (deployment: Deployment) => void;
}) {
  return (
    <tr class={cx(props.current && "bg-console-accent-soft/40")}>
      <td style={props.current ? { "box-shadow": `inset 3px 0 0 ${props.environmentColor}` } : undefined}>
        <PrimaryLink href={deploymentHref(props.deployment.id)} title={props.deployment.version}>{shortVersion(props.deployment.version)}</PrimaryLink>
      </td>
      <td>
        <Show when={props.current} fallback={<EmptyCell />}>
          <StatusBadge resource="deployment" status="current" />
        </Show>
      </td>
      <td><IDText value={props.deployment.bundle_digest} /></td>
      <td><RelativeTime value={props.deployment.created_at} /></td>
      <RowActions
        label={`Actions for ${shortVersion(props.deployment.version)}`}
        items={props.canPromote ? [{ label: "Promote", onSelect: () => props.onPromote(props.deployment) }] : []}
      />
    </tr>
  );
}

export function Deployments() {
  const scope = useScope();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const enabled = () => !!projectID() && !!environmentID();
  const resourceScope = () => ({ projectID: projectID(), environmentID: environmentID() });
  const me = useMe();

  const deployments = createInfiniteQuery(() => ({
    queryKey: ["deployments", "list", projectID(), environmentID()],
    queryFn: ({ pageParam }) => listDeployments(resourceScope(), { cursor: pageParam || undefined, limit: 50 }),
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    enabled: enabled(),
    retry: false,
  }));
  const current = createQuery(() => ({
    queryKey: ["deployments", "current", projectID(), environmentID()],
    queryFn: () => getCurrentDeployment(resourceScope()),
    enabled: enabled(),
    retry: false,
  }));

  const items = createMemo(() => deployments.data?.pages.flatMap((page) => page.deployments) ?? []);
  const currentID = createMemo(() => current.data?.id ?? "");
  const environmentColor = createMemo(() => {
    const environment = scope.selectedEnvironment();
    return environment?.color_hex || defaultEnvironmentColor(environment?.environment);
  });
  const [promoting, setPromoting] = createSignal<Deployment | null>(null);

  return (
    <Page>
      <PageHeader title="Deployments" />
      <QueryState
        query={deployments}
        pending={<TableSkeleton label="Loading deployments" columns={COLUMNS} minWidth="min-w-180" />}
        error={deploymentsErrorMessage}
        empty={{
          when: (data) => data.pages.every((page) => page.deployments.length === 0),
          title: "No deployments yet.",
          hint: <>Run <code>helmr deploy</code> against this environment to create the first one.</>,
        }}
      >
        {() => (
          <DataTable
            columns={COLUMNS}
            minWidth="min-w-180"
            footer={<LoadMore count={items().length} noun={{ one: "deployment", other: "deployments" }} hasMore={deployments.hasNextPage} loading={deployments.isFetchingNextPage} failed={deployments.isFetchNextPageError} onLoadMore={() => void deployments.fetchNextPage()} />}
          >
            <For each={items()}>
              {(deployment) => (
                <DeploymentRow
                  deployment={deployment}
                  current={deployment.id === currentID()}
                  environmentColor={environmentColor()}
                  canPromote={canPromoteDeployment({
                    permitted: hasPermission(me.data, "deployment.promote", {projectID:projectID(),environmentID:environmentID()}),
                    currentLoaded: current.isSuccess,
                    isCurrent: deployment.id === currentID(),
                  })}
                  onPromote={setPromoting}
                />
              )}
            </For>
          </DataTable>
        )}
      </QueryState>

      <Show when={promoting()}>
        {(deployment) => (
          <PromoteDeploymentModal
            deployment={deployment()}
            projectID={projectID()}
            environmentID={environmentID()}
            environment={scope.selectedEnvironment()}
            onClose={() => setPromoting(null)}
          />
        )}
      </Show>
    </Page>
  );
}
