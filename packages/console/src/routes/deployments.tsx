import { A } from "@solidjs/router";
import { createInfiniteQuery, createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { canPromoteDeployment, PromoteDeploymentModal } from "../features/deployments/PromoteDeploymentModal";
import { defaultEnvironmentColor } from "../features/projects/display";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { getCurrentDeployment, listDeployments, type Deployment } from "../lib/deployments";
import { useScope } from "../lib/scope";
import { ActionMenu } from "../ui/ActionMenu";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { cx, ui } from "../ui/styles";

function deploymentsErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "forbidden") {
    return "You do not have permission to view Deployments.";
  }
  if (error instanceof ApiError) return error.message;
  return "Could not load Deployments.";
}

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
        <A href={deploymentHref(props.deployment.id)} class="font-medium text-console-text hover:text-console-accent">
          {props.deployment.version}
        </A>
      </td>
      <td>
        <Show when={props.current} fallback={<span class="text-console-faint">—</span>}>
          <StatusBadge resource="deployment" status="current" />
        </Show>
      </td>
      <td><IDText value={props.deployment.bundle_digest} /></td>
      <td><RelativeTime value={props.deployment.created_at} /></td>
      <td class={ui.actionsCell}>
        <Show when={props.canPromote}>
          <ActionMenu
            label={`Actions for ${props.deployment.version}`}
            items={[{ label: "Promote", onSelect: () => props.onPromote(props.deployment) }]}
          />
        </Show>
      </td>
    </tr>
  );
}

export function Deployments() {
  const scope = useScope();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const enabled = () => !!projectID() && !!environmentID();
  const resourceScope = () => ({ projectID: projectID(), environmentID: environmentID() });
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));

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
    return environment?.color_hex || defaultEnvironmentColor(environment?.slug);
  });
  const [promoting, setPromoting] = createSignal<Deployment | null>(null);

  return (
    <section class={ui.page}>
      <PageHeader
        title="Deployments"
        subtitle="Immutable bundles deployed to the selected environment. Promotion changes which Deployment is current."
      />

      <Show when={deployments.isError}>
        <StatePanel error={deploymentsErrorMessage(deployments.error)} />
      </Show>

      <Show when={!deployments.isPending} fallback={<StatePanel loading="Loading Deployments..." />}>
        <Show
          when={items().length > 0}
          fallback={<StatePanel empty="No Deployments yet." hint={<>Run <code>helmr deploy</code> against this environment to create the first one.</>} />}
        >
          <DataTable columns={["Version", "Status", "Digest", "Created", { label: "Actions", srOnly: true }]} minWidth="min-w-180">
            <For each={items()}>
              {(deployment) => (
                <DeploymentRow
                  deployment={deployment}
                  current={deployment.id === currentID()}
                  environmentColor={environmentColor()}
                  canPromote={canPromoteDeployment({
                    permitted: hasPermission(me.data, "tasks.deploy"),
                    currentLoaded: current.isSuccess,
                    isCurrent: deployment.id === currentID(),
                  })}
                  onPromote={setPromoting}
                />
              )}
            </For>
          </DataTable>
          <Show when={deployments.hasNextPage}>
            <div class={ui.actionRow}>
              <button
                type="button"
                class={ui.secondaryButton}
                disabled={deployments.isFetchingNextPage}
                onClick={() => void deployments.fetchNextPage()}
              >
                {deployments.isFetchingNextPage ? "Loading..." : "Load more"}
              </button>
            </div>
          </Show>
        </Show>
      </Show>

      <Show when={promoting()}>
        {(deployment) => (
          <PromoteDeploymentModal
            deployment={deployment()}
            projectID={projectID()}
            environmentID={environmentID()}
            environmentName={scope.selectedEnvironment()?.name}
            onClose={() => setPromoting(null)}
          />
        )}
      </Show>
    </section>
  );
}
