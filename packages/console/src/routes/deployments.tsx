import { A } from "@solidjs/router";
import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { deploymentHref, shortDigest, shortID } from "../features/deployments/display";
import { defaultEnvironmentColor } from "../features/projects/display";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { getCurrentDeployment, listDeployments, promoteDeployment, type Deployment } from "../lib/deployments";
import { useScope } from "../lib/scope";
import { ConfirmModal } from "../ui/ConfirmModal";
import { cx, envDotStyle, ui } from "../ui/styles";

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
        <div class={ui.tableCellStack}>
          <A href={deploymentHref(props.deployment.id)} class="font-medium text-console-text hover:text-console-accent">
            {props.deployment.version}
          </A>
          <Show when={props.current}>
            <div class="flex items-center gap-1.5 font-mono text-[10.5px] font-medium uppercase tracking-[0.06em] text-console-subtle">
              <span class="inline-block size-1.5 shrink-0 rounded-full" style={envDotStyle(props.environmentColor)} aria-hidden="true" />
              current
            </div>
          </Show>
        </div>
      </td>
      <td><code title={props.deployment.bundle_digest}>{shortDigest(props.deployment.bundle_digest)}</code></td>
      <td><span class={ui.muted}>{formatRelative(props.deployment.created_at)}</span></td>
      <td><code>{shortID(props.deployment.id)}</code></td>
      <td class={ui.actionsCell}>
        <Show when={props.canPromote && !props.current}>
          <button type="button" class={ui.secondaryButton} onClick={() => props.onPromote(props.deployment)}>
            Promote
          </button>
        </Show>
      </td>
    </tr>
  );
}

export function Deployments() {
  const scope = useScope();
  const queryClient = useQueryClient();
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
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Deployments</h1>
          <p class={ui.pageSubtitle}>
            Immutable bundles deployed to the selected environment. Promotion changes which Deployment is current.
          </p>
        </div>
      </div>

      <Show when={deployments.isError}>
        <p class={ui.error} role="alert">{deploymentsErrorMessage(deployments.error)}</p>
      </Show>

      <Show when={!deployments.isPending} fallback={<p class={ui.muted}>Loading Deployments...</p>}>
        <Show
          when={items().length > 0}
          fallback={
            <div class={ui.emptyState}>
              <strong>No Deployments yet.</strong>
              <span>Run <code>helmr deploy</code> against this environment to create the first one.</span>
            </div>
          }
        >
          <div class={ui.tableWrap}>
            <table class="min-w-200">
              <thead>
                <tr>
                  <th>Version</th>
                  <th>Digest</th>
                  <th>Created</th>
                  <th>ID</th>
                  <th><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                <For each={items()}>
                  {(deployment) => (
                    <DeploymentRow
                      deployment={deployment}
                      current={deployment.id === currentID()}
                      environmentColor={environmentColor()}
                      canPromote={hasPermission(me.data, "tasks.deploy")}
                      onPromote={setPromoting}
                    />
                  )}
                </For>
              </tbody>
            </table>
          </div>
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
          <ConfirmModal
            title="Promote Deployment"
            confirmLabel="Promote"
            busyLabel="Promoting..."
            onClose={() => setPromoting(null)}
            onConfirm={async () => {
              await promoteDeployment(deployment().id, resourceScope());
              await queryClient.invalidateQueries({ queryKey: ["deployments"] });
              await queryClient.invalidateQueries({ queryKey: ["schedules"] });
            }}
            errorMessage={(error) => (error instanceof ApiError && error.code === "forbidden"
              ? "You do not have permission to promote Deployments."
              : error instanceof ApiError ? error.message : "Could not promote this Deployment.")}
          >
            <strong>{deployment().version}</strong> becomes the current Deployment for{" "}
            <strong>{scope.selectedEnvironment()?.name ?? "this environment"}</strong>. New Runs use its definitions and its schedules take effect.
          </ConfirmModal>
        )}
      </Show>
    </section>
  );
}
