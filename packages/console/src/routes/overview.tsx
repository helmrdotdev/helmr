import { A } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { definitionHref } from "../lib/definitions";
import { errorMessage } from "../lib/error";
import { getCurrentDeployment } from "../lib/deployments";
import { useScope } from "../lib/scope";
import { listSessions, sessionConsolePath } from "../lib/sessions";
import { CodeBlock } from "../ui/CodeBlock";
import { DataTable, PrimaryLink } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Page, Section } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { EmptyState, QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

const overviewError = (error: unknown) => errorMessage(error, "Could not load the environment overview.");

const CLI_SETUP = `helmr init --dir ./my-agent
cd ./my-agent
bun install
helmr deploy . --project PROJECT --env ENVIRONMENT`;

function CliOnboarding() {
  return (
    <Panel title="Set up your first Agent" variant="window">
      <p class={ui.muted}>Create an Agent project, install its dependencies, and deploy it to this environment.</p>
      <CodeBlock code={CLI_SETUP} copy label="CLI setup commands" />
      <p class={ui.muted}>Then create a Session through the CLI or a connected Slack app. Browse the current Deployment to inspect its Agents. Each Session retains its Turns, output, questions and controls.</p>
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
    <Page>
      <PageHeader title="Overview" subtitle="Agents and retained work in the selected environment." />
      <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
        <div class={ui.sectionStack}>
          <QueryState query={current} pending="Loading Deployment..." error={overviewError}>
            {(deployment) => (
              <Show when={deployment()} fallback={<CliOnboarding />}>
                {(record) => (
                  <Panel title="Current Deployment">
                    <div class="flex flex-wrap items-center gap-3">
                      <IDText value={record().id} mode="link" href={deploymentHref(record().id)} />
                      <span class={ui.muted}>Created <RelativeTime value={record().created_at} /></span>
                      <A class={ui.secondaryButton} href="/agents">View Agents</A>
                    </div>
                  </Panel>
                )}
              </Show>
            )}
          </QueryState>
          <Section title="Recent Sessions" description="Open a Session to inspect its Turns, answer questions or manage held work."
            actions={<A class={ui.secondaryButton} href="/sessions">All Sessions</A>}>
            <QueryState
              query={sessions}
              pending="Loading Sessions..."
              error={overviewError}
              empty={{ when: (data) => data.sessions.length === 0, title: "No Sessions yet.", hint: "Create a Session through the CLI or a connected Slack app." }}
            >
              {(data) => (
                <DataTable columns={["Session", "Agent", "Status", "Holds", "Created"]}>
                  <For each={data().sessions}>{session => (
                    <tr>
                      <td><IDText value={session.id} mode="link" href={sessionConsolePath(session.id)} /></td>
                      <td><PrimaryLink href={definitionHref("agents", session.agent, session.deployment_id)}>{session.agent}</PrimaryLink></td>
                      <td><StatusBadge resource="session" status={session.status} /></td>
                      <td>{session.holds.length ? `${session.holds.length} active` : "None"}</td>
                      <td><RelativeTime value={session.created_at} /></td>
                    </tr>
                  )}</For>
                </DataTable>
              )}
            </QueryState>
          </Section>
        </div>
      </Show>
    </Page>
  );
}
