import { createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, createSignal, on, onCleanup, Show, type JSX } from "solid-js";
import type { Failure } from "../../lib/error";
import { usePermission } from "../../lib/me";
import { useScope } from "../../lib/scope";
import { authorizeAgentSlackConnection, beginAgentSlackConnection, disconnectAgentSlackConnection, getAgentSlackConnection, saveSlackAppCredentials, type AgentSlackAddress, type AgentSlackConnection as Connection } from "../../lib/slack";
import { JsonView } from "../../ui/PayloadView";
import type { JsonValue } from "../../../../../sdk/typescript/src/contract";
import { ConfirmDialog } from "../../ui/Dialog";
import { Field, FormActions, SecretInput, TextInput } from "../../ui/Field";
import { Notice } from "../../ui/Notice";
import { DangerZone, DetailLayout, KeyValue, KeyValueList, Panel } from "../../ui/Panel";
import { QueryState } from "../../ui/QueryState";
import { PageSkeleton } from "../../ui/Skeleton";
import { StatusBadge } from "../../ui/StatusBadge";
import { Steps, type Step } from "../../ui/Steps";
import { statusLabel } from "../../ui/label";
import { cx, ui } from "../../ui/styles";
import { environmentChip } from "../projects/display";
import { AppIconPicker } from "./AppIconPicker";
import { slackFailure, slackNotConfigured } from "./failure";
import { SlackFailureNotice, SlackNotSetUp } from "./SlackFailure";

// The manifest parts people check against Slack: the scopes and the event URL.
const MANIFEST_OPEN: ReadonlySet<string> = new Set(["oauth_config", "oauth_config.scopes", "settings", "settings.event_subscriptions"]);

type Credentials = import("../../lib/slack").SlackAppCredentials;

const MANAGERS = "Members with Agent connection permissions";

// Reading, configuring and disconnecting a connection have separate permissions.
export function AgentSlackConnection(props: { agentName: string }) {
  const permission = usePermission();
  const scope = useScope();
  const environmentName = () => scope.selectedEnvironment()?.name ?? "this environment";
  return (
    <Show
      when={permission.has("agent_connections.read", {projectID:scope.selectedProjectID(), environmentID:scope.selectedEnvironmentID(), agent:props.agentName})}
      fallback={
        <DetailLayout rail={<ConnectionFacts environmentName={environmentName()} />}>
          <Panel title="Slack connection">
            <p class="m-0 text-body">{MANAGERS} manage this connection. Ask one of them to connect, reauthorize or disconnect it.</p>
            <p class={cx(ui.muted, "m-0")}>Each agent can have its own Slack app in this environment. Its app name and avatar identify it in conversations.</p>
          </Panel>
        </DetailLayout>
      }
    >
      <ManagedConnection agentName={props.agentName} projectID={scope.selectedProjectID()} environmentID={scope.selectedEnvironmentID()} environmentName={environmentName()} />
    </Show>
  );
}

// The connection's known facts; nothing that would read "—". The scope is
// shown until the app is installed, after which the page header says it.
function ConnectionFacts(props: { environmentName: string; installed?: boolean; children?: JSX.Element }) {
  return (
    <div class="flex flex-col gap-1.5">
      <KeyValueList title="Connection">
        {props.children}
        <Show when={!props.installed}>
          <KeyValue label="Scope">This agent in {props.environmentName}. Kept across deployments.</KeyValue>
        </Show>
      </KeyValueList>
      <p class={cx(ui.muted, "m-0 text-chrome")}>Managed by {MANAGERS.toLowerCase()}</p>
    </div>
  );
}

function ManagedConnection(props: { agentName: string; projectID: string; environmentID: string; environmentName: string }) {
  const scope = useScope();
  const permission = usePermission();
  const can = (action: string) => permission.has(`agent_connections.${action}`, {projectID: props.projectID, environmentID: props.environmentID, agent: props.agentName});
  const address = (): AgentSlackAddress => ({ projectID: props.projectID, environmentID: props.environmentID, agentName: props.agentName });
  const iconSeed = () => `${props.projectID}/${props.environmentID}/${props.agentName}`;
  const state = createQuery(() => ({ queryKey: ["agent-slack", props.projectID, props.environmentID, props.agentName], queryFn: () => getAgentSlackConnection(address()), retry: false, gcTime: 0, refetchOnWindowFocus: false, refetchInterval: 15000 }));
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<Failure>();
  const [removing, setRemoving] = createSignal<Connection | null>(null);
  createEffect(on(() => JSON.stringify(address()), () => { setError(undefined); setRemoving(null); }, { defer: true }));
  let mounted = true;
  onCleanup(() => { mounted = false; });
  const run = async (action: () => Promise<unknown>) => {
    setBusy(true); setError(undefined);
    try { await action(); if (mounted) await state.refetch(); }
    catch (failure) { if (mounted) setError(slackFailure(failure, "The connection could not be updated.")); throw failure; }
    finally { if (mounted) setBusy(false); }
  };
  const attempt = (action: () => Promise<unknown>) => { void run(action).catch(() => undefined); };
  const saveCredentials = (connection: Connection, credentials: Credentials) => run(() => saveSlackAppCredentials(address(), connection.id, credentials));
  const authorize = (connection: Connection) => attempt(async () => {
    const result = await authorizeAgentSlackConnection(address(), connection.id, window.location.pathname + window.location.search);
    if (mounted) window.location.assign(result.redirect_url);
  });

  return (
    <Show when={!slackNotConfigured(state.error)} fallback={<Panel title="Slack connection"><SlackNotSetUp failure={slackFailure(state.error, "")} consequence="Agents cannot be connected to Slack until it is set up." /></Panel>}>
    <QueryState query={state} pending={<PageSkeleton label="Loading Slack connection" sections={["Slack connection"]} />} error={(failure) => slackFailure(failure, "The connection could not be loaded.").message}>
      {(data) => (
        <DetailLayout rail={
          <div class="flex flex-col gap-4">
            <ConnectionFacts environmentName={props.environmentName} installed={!!data().connection?.installation_id}>
              <KeyValue label="Delivery">{data().transport === "socket" ? "Socket Mode" : "HTTP callbacks"} · Selected by the deployment operator</KeyValue>
              <KeyValue label="Authorization">
                <Show when={data().connection} fallback="Not connected">{(connection) => <StatusBadge resource="slack_connection" status={connection().status} />}</Show>
              </KeyValue>
              <Show when={data().connection?.socket}>{(health) => <KeyValue label="Event connection">{statusLabel(health().status)}</KeyValue>}</Show>
              <Show when={data().connection?.app_name}>{(name) => <KeyValue label="App">{name()}</KeyValue>}</Show>
              <Show when={data().connection?.workspace_name}>{(name) => <KeyValue label="Workspace">{name()}</KeyValue>}</Show>
              <Show when={data().connection?.app_id}>{(id) => <KeyValue label="App ID"><span class="font-mono">{id()}</span></KeyValue>}</Show>
            </ConnectionFacts>
            <Show when={data().connection}>
              {(connection) => (
                <Show
                  when={connection().installation_id}
                  fallback={
                    <DangerZone title="Discard setup" action={<button type="button" class={ui.dangerOutlineButton} disabled={busy() || !can("delete")} onClick={() => setRemoving(connection())}>Discard setup</button>}>
                      Frees this agent’s connection. The app in Slack is not deleted.
                    </DangerZone>
                  }
                >
                  <DangerZone title="Disconnect Slack" action={<button type="button" class={ui.dangerOutlineButton} disabled={busy() || !can("delete")} onClick={() => setRemoving(connection())}>Disconnect</button>}>
                    Stops Slack input, answers and output in this app’s threads.
                  </DangerZone>
                </Show>
              )}
            </Show>
          </div>
        }>
          <Show when={error()}>{(failure) => <SlackFailureNotice failure={failure()} />}</Show>
          <Show
            when={data().connection?.id}
            keyed
            fallback={
              <SetupPanel status="Not connected" step={1}>
                <Steps label="Slack setup" steps={setupSteps({
                  create: { state: "current", content: <>
                    <p class={ui.muted}>Each agent has one Slack app in this environment. Its app name and avatar identify it in conversations. Start to get the app manifest for <strong>{props.agentName}</strong>.</p>
                    <div><button type="button" class={ui.button} disabled={busy() || !can("configure")} onClick={() => attempt(() => beginAgentSlackConnection(address()))}>Connect Slack</button></div>
                  </> },
                })} />
              </SetupPanel>
            }
          >
            {(_connectionID) => <Show when={data().connection}>{(connection) => (
              <Show when={connection().status === "setup_incomplete"} fallback={
                <InstalledConnection
                  manifest={data().manifest}
                  connection={connection()}
                  agentName={props.agentName}
                  iconSeed={iconSeed()}
                  busy={busy()}
                  canAuthorize={can("configure")}
                  canCredentials={can("configure")}
                  authorize={() => authorize(connection())}
                  save={(credentials) => saveCredentials(connection(), credentials)}
                />
              }>
                <Setup
                  connection={connection()}
                  manifest={data().manifest}
                  createAppURL={data().create_app_url}
                  agentName={props.agentName}
                  iconSeed={iconSeed()}
                  busy={busy()}
                  canAuthorize={can("configure")}
                  canCredentials={can("configure")}
                  authorize={() => authorize(connection())}
                  save={(credentials) => saveCredentials(connection(), credentials)}
                />
              </Show>
            )}</Show>}
          </Show>
          <Show when={removing()}>
            {(connection) => (
              <ConfirmDialog
                title={connection().installation_id ? "Disconnect Slack" : "Discard setup"}
                confirmLabel={connection().installation_id ? "Disconnect Slack" : "Discard setup"}
                busyLabel={connection().installation_id ? "Disconnecting…" : "Discarding…"}
                tone="danger"
                target={connection().app_name || props.agentName}
                environment={environmentChip(scope.selectedEnvironment())}
                errorMessage={(failure) => slackFailure(failure, "The connection could not be updated.").message}
                onClose={() => setRemoving(null)}
                onConfirm={async () => {
                  await disconnectAgentSlackConnection(address(), connection().id);
                  await state.refetch();
                }}
              >
                {connection().installation_id
                  ? "Disconnecting stops Slack input, answers and output in existing threads. Work can continue through the CLI or API. Reconnecting creates a new connection; existing threads stay disconnected and triggers require re-promotion."
                  : "Discard this setup to release the agent’s connection slot. The app in Slack is not deleted."}
              </ConfirmDialog>
            )}
          </Show>
        </DetailLayout>
      )}
    </QueryState>
    </Show>
  );
}

type StepState = Pick<Step, "state" | "content" | "description">;

function setupSteps(states: { create: StepState; install?: StepState | undefined }): Step[] {
  const upcoming: StepState = { state: "upcoming" };
  return [
    { title: "Create the Slack app", description: "From the manifest, with this agent’s name and icon, then save its credentials.", ...states.create },
    { title: "Install in your workspace", description: "Opens Slack to authorize, then returns here.", ...(states.install ?? upcoming) },
    { title: "Invite the app to a channel", description: "Mention it to start a session.", ...upcoming },
  ];
}

function SetupPanel(props: { status: string; step?: number; children: JSX.Element }) {
  return (
    <Panel title={props.step ? `Slack setup ${props.step} of 3` : "Slack connection"} actions={<span class="font-mono text-chrome text-console-muted">{props.status}</span>}>
      {props.children}
    </Panel>
  );
}

function Setup(props: {
  connection: Connection;
  manifest: JsonValue | undefined;
  createAppURL: string | undefined;
  agentName: string;
  iconSeed: string;
  busy: boolean;
  canAuthorize: boolean;
  canCredentials: boolean;
  authorize: () => void;
  save: (credentials: Credentials) => Promise<void>;
}) {
  // Saved credentials are what show the app exists in Slack. A memo keeps the
  // step, and any credential draft, across polls that change nothing.
  const step = createMemo(() => props.connection.credentials_configured ? 2 : 1);
  return (
    <SetupPanel status="Setup incomplete" step={step()}>
      <Steps label="Slack setup" steps={setupSteps({
        create: step() === 1
          ? { state: "current", content: <div class={ui.blockStack}>
              <Show when={props.createAppURL}>{(url) => <div><a class={ui.button} href={url()} target="_blank" rel="noopener noreferrer">Create app in Slack</a></div>}</Show>
              <JsonView value={props.manifest ?? {}} label="App manifest" kind="manifest" open={MANIFEST_OPEN} />
              <AppIconPicker seed={props.iconSeed} agentName={props.agentName} />
              <p class={ui.muted}>Copy the app credentials from the Slack app settings. Secrets are never shown again.</p>
              <CredentialForm connection={props.connection} busy={props.busy || !props.canCredentials} save={props.save} />
            </div> }
          : { state: "done" },
        install: step() === 2
          ? { state: "current", content: <div class={ui.blockStack}>
              <div><button type="button" class={ui.button} disabled={props.busy || !props.canAuthorize} onClick={() => props.authorize()}>Install app in Slack</button></div>
              <Disclosure summary="Replace app credentials"><CredentialForm connection={props.connection} busy={props.busy || !props.canCredentials} save={props.save} /></Disclosure>
            </div> }
          : undefined,
      })} />
    </SetupPanel>
  );
}

function InstalledConnection(props: {
  manifest: JsonValue | undefined;
  connection: Connection;
  agentName: string;
  iconSeed: string;
  busy: boolean;
  canAuthorize: boolean;
  canCredentials: boolean;
  authorize: () => void;
  save: (credentials: Credentials) => Promise<void>;
}) {
  const reauthorize = () => <button type="button" class={ui.button} disabled={props.busy || !props.canAuthorize} onClick={() => props.authorize()}>Reauthorize in Slack</button>;
  return (
    <SetupPanel status={props.connection.transport === "socket" && props.connection.status === "connected" ? "Authorized" : statusLabel(props.connection.status)}>
      <Show when={props.connection.status === "configuration_mismatch"}><Notice tone="warning">The deployment’s Slack delivery mode changed. Disconnect this connection and set it up again using the current configuration.</Notice></Show>
      <Show when={props.connection.socket && props.connection.status === "connected" && props.connection.socket.status !== "connected"}><Notice tone="warning">Slack is authorized, but event delivery {props.connection.socket?.status === "action_required" ? "needs attention" : `is ${statusLabel(props.connection.socket?.status ?? "disconnected").toLowerCase()}`}. {props.connection.socket?.status === "action_required" ? "Check the app-level token and its connections:write scope, or ask the deployment operator to check connectivity." : "The deployment will retry automatically. Ask the deployment operator if this persists."}</Notice></Show>
      <Show when={props.connection.status === "connected" && (!props.connection.socket || props.connection.socket.status === "connected")}>
        <Notice tone="success">Connected to {props.connection.workspace_name || "the workspace"} as {props.connection.app_name || props.agentName}. Invite this app to a channel and mention it to start a session.<Show when={props.connection.direct_messages_authorized}> With the current app manifest applied, you can also message it directly without a mention. Start a new message for a new session; reply in its thread to continue.</Show></Notice>
      </Show>
      <Show when={props.connection.status === "connected" && !props.connection.direct_messages_authorized}>
        <Notice tone="warning">Direct messages need additional Slack permissions. Update this app’s manifest below, then choose Reauthorize in Slack for the same workspace. Channel conversations remain available.</Notice>
      </Show>
      <p class={ui.muted}>Starting a conversation requires Create sessions for this agent. Reading sessions includes every conversation for the assigned agent, including private channels and DMs. Sending input, answering questions, resuming and stopping each require their own permission. Channel input also requires channel membership; DM delivery requires the original recipient to retain session read access. Authorized input can produce replies in the original DM. The app’s online indicator shows availability in Slack, not server health.</p>
      <Show when={props.connection.status === "reauthorization_required"}>
        <Notice tone="warning" action={reauthorize()}>Slack authorization needs repair. Update this app’s manifest below first. Reauthorize this same app and workspace to continue. Earlier unsent output will not be backfilled.</Notice>
      </Show>
      <Show when={props.connection.status === "disconnected"}>
        <Notice tone="danger" role="status">Slack no longer accepts this app’s installation. Disconnect it here, then connect Slack again to create a new connection.</Notice>
      </Show>
      <Show when={props.connection.status === "connected"}>
        <div class={ui.buttonGroup}>{reauthorize()}</div>
      </Show>
      <Show when={props.connection.status !== "disconnected" && props.connection.status !== "configuration_mismatch"}>
        <Disclosure summary="Update app credentials"><CredentialForm connection={props.connection} busy={props.busy || !props.canCredentials} save={props.save} /></Disclosure>
      </Show>
      <Disclosure summary="Update app manifest">
        <p class={ui.muted}>In this app’s Slack settings, replace its App Manifest with the current manifest below. Save it, then reauthorize if permissions changed. Keep token rotation enabled; Slack may show a setup notice for it.</p>
        <JsonView value={props.manifest ?? {}} label="App manifest" kind="manifest" open={MANIFEST_OPEN} />
      </Disclosure>
      <Disclosure summary="Download app icon"><AppIconPicker seed={props.iconSeed} agentName={props.agentName} /></Disclosure>
    </SetupPanel>
  );
}

function Disclosure(props: { summary: string; children: JSX.Element }) {
  return (
    <details>
      <summary class="cursor-pointer font-mono text-chrome">{props.summary}</summary>
      <div class="mt-2">{props.children}</div>
    </details>
  );
}

function CredentialForm(props: { connection: Connection; busy: boolean; save: (credentials: Credentials) => Promise<void> }) {
  const [client, setClient] = createSignal(props.connection.client_id || "");
  const [secret, setSecret] = createSignal("");
  const [signing, setSigning] = createSignal("");
  const clear = () => { setSecret(""); setSigning(""); };
  return (
    <form onSubmit={(event) => {
      event.preventDefault();
      void props.save({ client_id: client().trim(), client_secret: secret().trim(), ...(props.connection.transport === "socket" ? { app_token: signing().trim() } : { signing_secret: signing().trim() }) }).then(clear, () => undefined);
    }}>
      <div class="grid grid-cols-3 items-start gap-x-3 max-[960px]:grid-cols-1">
        <Field label="Client ID" hint={props.connection.credentials_configured ? "Fixed for this app." : undefined}>
          <TextInput class="font-mono" required value={client()} readOnly={props.connection.credentials_configured} disabled={props.busy} autocomplete="off" spellcheck={false} onInput={(event) => setClient(event.currentTarget.value)} />
        </Field>
        <Field label="Client secret">
          <SecretInput required value={secret()} disabled={props.busy} onInput={(event) => setSecret(event.currentTarget.value)} />
        </Field>
        <Field label={props.connection.transport === "socket" ? "App-level token" : "Signing secret"} hint={props.connection.transport === "socket" ? "In Basic Information → App-Level Tokens, generate a token with connections:write. Use the same Slack app." : undefined}>
          <SecretInput required value={signing()} disabled={props.busy} onInput={(event) => setSigning(event.currentTarget.value)} />
        </Field>
      </div>
      <FormActions submitLabel="Save app credentials" busyLabel="Saving…" busy={props.busy} cancelLabel="Clear" onCancel={clear} />
    </form>
  );
}
