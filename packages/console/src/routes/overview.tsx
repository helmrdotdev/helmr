import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { ApiError } from "../lib/api";
import { getCurrentDeployment } from "../lib/deployments";
import { useScope } from "../lib/scope";
import { listSessions, sessionConsolePath } from "../lib/sessions";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { SectionHeader } from "../ui/SectionHeader";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

function errorMessage(error: unknown): string {
  return error instanceof ApiError ? error.message : "Could not load the environment overview.";
}

function CliOnboarding() {
  return (
    <Panel title="Set up your first Agent">
      <p class={ui.muted}>Create an Agent project, install its dependencies, and deploy it to this environment.</p>
      <pre class="overflow-x-auto border border-console-border bg-console-bg-panel p-3 font-mono text-xs">{`helmr init --dir ./my-agent
cd ./my-agent
bun install
helmr deploy . --project PROJECT --env ENVIRONMENT`}</pre>
      <p class={ui.muted}>Then open the current Deployment to start an Agent. Each Session retains its Turns, output, questions and controls.</p>
    </Panel>
  );
}

export function Overview() {
  const scope = useScope();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const enabled = () => !!projectID() && !!environmentID();
  const current = createQuery(() => ({
    queryKey: ["deployments", "current", projectID(), environmentID()],
    queryFn: () => getCurrentDeployment({ projectID: projectID(), environmentID: environmentID() }),
    enabled: enabled(),
    retry: false,
    refetchInterval: 10_000,
  }));
  const sessions = createQuery(() => ({
    queryKey: ["sessions", "overview", projectID(), environmentID()],
    queryFn: () => listSessions({ projectID: projectID(), environmentID: environmentID(), limit: 10 }),
    enabled: enabled(),
    retry: false,
    refetchInterval: 5_000,
  }));
  return (
    <section class={ui.page}>
      <PageHeader title="Overview" subtitle="Agents and retained work in the selected environment." />
      <Show when={enabled()} fallback={<StatePanel empty="Select a project and environment." />}>
        <div class="flex flex-col gap-6">
          <Show when={current.isError}><StatePanel error={errorMessage(current.error)} /></Show>
          <Show when={current.isPending}><StatePanel loading="Loading Deployment..." /></Show>
          <Show when={current.isSuccess}>
            <Show when={current.data} fallback={<CliOnboarding />}>
              {deployment => (
                <Panel title="Current Deployment">
                  <div class="flex flex-wrap items-center gap-3">
                    <IDText value={deployment().id} mode="link" href={deploymentHref(deployment().id)} />
                    <span class={ui.muted}>Created <RelativeTime value={deployment().created_at} /></span>
                    <A class={ui.secondaryButton} href={deploymentHref(deployment().id)}>View Agents</A>
                  </div>
                </Panel>
              )}
            </Show>
          </Show>
          <section>
            <SectionHeader title="Recent Sessions" subtitle="Open a Session to inspect its Turns, answer questions or manage held work."
              actions={<A class={ui.secondaryButton} href="/sessions">All Sessions</A>} />
            <Show when={sessions.isError}><StatePanel error={errorMessage(sessions.error)} /></Show>
            <Show when={sessions.isPending}><StatePanel loading="Loading Sessions..." /></Show>
            <Show when={sessions.isSuccess}>
              <Show when={sessions.data?.sessions.length} fallback={<StatePanel empty="No Sessions yet." hint="Start an Agent from the current Deployment." />}>
                <DataTable columns={["Session", "Agent", "Status", "Holds", "Created"]}>
                  <For each={sessions.data?.sessions}>{session => (
                    <tr>
                      <td><IDText value={session.id} mode="link" href={sessionConsolePath(session.id, projectID(), environmentID())} /></td>
                      <td><IDText value={session.agent_id} /></td>
                      <td><StatusBadge resource="session" status={session.status} /></td>
                      <td>{session.holds.length ? `${session.holds.length} active` : "None"}</td>
                      <td><RelativeTime value={session.created_at} /></td>
                    </tr>
                  )}</For>
                </DataTable>
              </Show>
            </Show>
          </section>
        </div>
      </Show>
    </section>
  );
}
