import { createQuery } from "@tanstack/solid-query";
import { createSignal, onCleanup, Show } from "solid-js";
import { errorMessage } from "../../lib/error";
import { useScope } from "../../lib/scope";
import { authorizeAgentSlackConnection, beginAgentSlackConnection, disconnectAgentSlackConnection, getAgentSlackConnection, saveSlackAppCredentials, type AgentSlackAddress, type AgentSlackConnection } from "../../lib/slack";
import { CodeBlock } from "../../ui/CodeBlock";
import { ConfirmDialog, Dialog } from "../../ui/Dialog";
import { Field, FormActions, SecretInput, TextInput } from "../../ui/Field";
import { IDText } from "../../ui/IDText";
import { Notice } from "../../ui/Notice";
import { KeyValue, KeyValueList } from "../../ui/Panel";
import { QueryState } from "../../ui/QueryState";
import { ui } from "../../ui/styles";
import { environmentChip } from "../projects/display";
import { AppIconPicker } from "./AppIconPicker";

export function AgentConnectionModal(props: AgentSlackAddress & { onClose: () => void }) {
  const scope = useScope();
  const address = { projectID: props.projectID, environmentID: props.environmentID, agentName: props.agentName };
  const iconSeed = `${props.projectID}/${props.environmentID}/${props.agentName}`;
  const state = createQuery(() => ({ queryKey: ["agent-slack", address.projectID, address.environmentID, address.agentName], queryFn: () => getAgentSlackConnection(address), retry: false, gcTime: 0, refetchOnWindowFocus: false }));
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string>();
  const [disconnecting, setDisconnecting] = createSignal<AgentSlackConnection | null>(null);
  let mounted = true;
  onCleanup(() => { mounted = false; });
  const run = async (action: () => Promise<unknown>) => {
    setBusy(true); setError(undefined);
    try { await action(); if (mounted) await state.refetch(); }
    catch (failure) { if (mounted) setError(errorMessage(failure, "The connection could not be updated.")); }
    finally { if (mounted) setBusy(false); }
  };
  const authorize = (id: string) => run(async () => {
    const result = await authorizeAgentSlackConnection(address, id, window.location.pathname + window.location.search);
    if (mounted) window.location.assign(result.redirect_url);
  });
  return <Dialog title={`Slack · ${props.agentName}`} onClose={props.onClose} closeDisabled={busy()}>
    <div class={ui.windowStack}>
      <Show when={error()}>{(message) => <Notice tone="danger">{message()}</Notice>}</Show>
      <QueryState query={state} pending="Loading connection..." error={() => "The connection could not be loaded. An organization owner or admin can manage it."}>
        {(data) => (
          <Show when={data().connection} keyed fallback={<>
            <p class="m-0 text-body">Connect this Agent to Slack with its own app. Its app name and avatar identify it in conversations.</p>
            <p class={ui.muted}>Each Agent has one Slack connection in this Environment. Invite the app to the channels where you want to use it.</p>
            <div class={ui.modalActions}><button type="button" class={ui.button} disabled={busy()} onClick={() => void run(() => beginAgentSlackConnection(address))}>Connect Slack</button></div>
          </>}>{(connection) => <>
            <Show when={connection.status === "setup_incomplete"} fallback={<>
              <KeyValueList title="Connection">
                <KeyValue label="App">{connection.app_name || props.agentName}</KeyValue>
                <KeyValue label="Workspace">{connection.workspace_name}</KeyValue>
                <KeyValue label="App ID"><IDText value={connection.app_id ?? ""} /></KeyValue>
                <KeyValue label="Workspace ID"><IDText value={connection.team_id ?? ""} /></KeyValue>
              </KeyValueList>
              <Show when={connection.status === "connected"}><Notice tone="success">Connected. Invite this app to a channel and mention it to start a conversation.</Notice></Show>
              <Show when={connection.status === "reauthorization_required"}><Notice tone="warning">Slack authorization needs repair. Reauthorize this same app and workspace to continue. Earlier unsent output will not be backfilled.</Notice></Show>
            </>}>
              <p class="m-0 text-body">Create a dedicated Slack app for <strong>{props.agentName}</strong>, then save its credentials here.</p>
              <p class={ui.muted}>The app’s Basic Information page contains these credentials. Slack can verify its event URL after they are saved; retry URL verification in Slack if needed.</p>
              <Show when={data().create_app_url}>{(url) => <div><a class={ui.secondaryButton} href={url()} target="_blank" rel="noopener noreferrer">Create app in Slack</a></div>}</Show>
              <AppIconPicker seed={iconSeed} agentName={props.agentName} />
              <details><summary class="cursor-pointer font-mono text-chrome">App manifest</summary><CodeBlock class="mt-2" code={JSON.stringify(data().manifest, null, 2)} copy label="App manifest" /></details>
            </Show>
            <Show when={connection.status === "setup_incomplete" || connection.status === "reauthorization_required"}>
              <CredentialForm connection={connection} busy={busy()} save={(credentials) => run(() => saveSlackAppCredentials(address, connection.id, credentials))} />
            </Show>
            <Show when={connection.status === "connected"}><details><summary class="cursor-pointer font-mono text-chrome">Download app icon</summary><div class="mt-2"><AppIconPicker seed={iconSeed} agentName={props.agentName} /></div></details></Show>
            <Show when={connection.status === "connected"}><details><summary class="cursor-pointer font-mono text-chrome">Update app credentials</summary><div class="mt-2"><CredentialForm connection={connection} busy={busy()} save={(credentials) => run(() => saveSlackAppCredentials(address, connection.id, credentials))} /></div></details></Show>
            <div class={ui.modalActions}>
              <button type="button" class={ui.dangerOutlineButton} disabled={busy()} onClick={() => setDisconnecting(connection)}>{connection.installation_id ? "Disconnect" : "Discard setup"}</button>
              <Show when={connection.credentials_configured}>
                <button type="button" class={ui.button} disabled={busy()} onClick={() => void authorize(connection.id)}>{connection.installation_id ? "Reauthorize in Slack" : "Install app in Slack"}</button>
              </Show>
            </div>
          </>}</Show>
        )}
      </QueryState>
    </div>
    <Show when={disconnecting()}>
      {(connection) => (
        <ConfirmDialog
          title={connection().installation_id ? "Disconnect Slack" : "Discard setup"}
          confirmLabel={connection().installation_id ? "Disconnect Slack" : "Discard setup"}
          busyLabel={connection().installation_id ? "Disconnecting..." : "Discarding..."}
          cancelLabel="Keep connection"
          tone="danger"
          target={connection().app_name || props.agentName}
          environment={environmentChip(scope.selectedEnvironment())}
          errorMessage={(failure) => errorMessage(failure, "The connection could not be updated.")}
          onClose={() => setDisconnecting(null)}
          onConfirm={async () => {
            await disconnectAgentSlackConnection(address, connection().id);
            await state.refetch();
          }}
        >
          {connection().installation_id
            ? "Disconnecting stops Slack input, answers and output in existing threads. Work can continue through the CLI or API. Reconnecting creates a new connection; existing threads stay disconnected and triggers require re-promotion."
            : "Discard this setup to release the Agent’s connection slot. The app in Slack is not deleted."}
        </ConfirmDialog>
      )}
    </Show>
  </Dialog>;
}

function CredentialForm(props: { connection: AgentSlackConnection; busy: boolean; save: (credentials: { client_id: string; client_secret: string; signing_secret: string }) => Promise<void> }) {
  const [client, setClient] = createSignal(props.connection.client_id || "");
  const [secret, setSecret] = createSignal("");
  const [signing, setSigning] = createSignal("");
  const clear = () => { setSecret(""); setSigning(""); };
  return <form onSubmit={(event) => { event.preventDefault(); void props.save({ client_id: client().trim(), client_secret: secret().trim(), signing_secret: signing().trim() }).then(clear); }}>
    <Field label="Client ID">
      <TextInput class="font-mono" required value={client()} readOnly={props.connection.credentials_configured} disabled={props.busy} autocomplete="off" spellcheck={false} onInput={(event) => setClient(event.currentTarget.value)} />
    </Field>
    <Field label="Client secret">
      <SecretInput required value={secret()} disabled={props.busy} onInput={(event) => setSecret(event.currentTarget.value)} />
    </Field>
    <Field label="Signing secret">
      <SecretInput required value={signing()} disabled={props.busy} onInput={(event) => setSigning(event.currentTarget.value)} />
    </Field>
    <FormActions submitLabel="Save app credentials" busyLabel="Saving..." busy={props.busy} cancelLabel="Clear" onCancel={clear} />
  </form>;
}
