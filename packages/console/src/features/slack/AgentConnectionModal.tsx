import { createQuery } from "@tanstack/solid-query";
import { createSignal, onCleanup, Show } from "solid-js";
import { authorizeAgentSlackConnection, beginAgentSlackConnection, disconnectAgentSlackConnection, getAgentSlackConnection, saveSlackAppCredentials, type AgentSlackAddress, type AgentSlackConnection } from "../../lib/slack";
import { Dialog } from "../../ui/Dialog";
import { AppIconPicker } from "./AppIconPicker";
import { ui } from "../../ui/styles";

export function AgentConnectionModal(props: AgentSlackAddress & { onClose: () => void }) {
  const address = { projectID: props.projectID, environmentID: props.environmentID, agentName: props.agentName };
  const iconSeed = `${props.projectID}/${props.environmentID}/${props.agentName}`;
  const state = createQuery(() => ({ queryKey: ["agent-slack", address.projectID, address.environmentID, address.agentName], queryFn: () => getAgentSlackConnection(address), retry: false, gcTime: 0, refetchOnWindowFocus: false }));
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string>();
  const [confirmDisconnect, setConfirmDisconnect] = createSignal(false);
  let mounted = true;
  onCleanup(() => { mounted = false; });
  const run = async (action: () => Promise<unknown>) => {
    setBusy(true); setError(undefined);
    try { await action(); if (mounted) { setConfirmDisconnect(false); await state.refetch(); } }
    catch (failure) { if (mounted) setError(failure instanceof Error ? failure.message : "The connection could not be updated."); }
    finally { if (mounted) setBusy(false); }
  };
  const authorize = (id: string) => run(async () => {
    const result = await authorizeAgentSlackConnection(address, id, window.location.pathname + window.location.search);
    if (mounted) window.location.assign(result.redirect_url);
  });
  return <Dialog title={`Slack · ${props.agentName}`} onClose={props.onClose} closeDisabled={busy()}>
    <Show when={state.isPending}><p class={ui.muted}>Loading connection...</p></Show>
    <Show when={state.error}><p class={ui.error}>The connection could not be loaded. An organization owner or admin can manage it.</p></Show>
    <Show when={error()}><p class={ui.error} role="alert">{error()}</p></Show>
    <Show when={state.data}>
      <Show when={state.data?.connection} keyed fallback={<>
        <p>Connect this Agent to Slack with its own app. Its app name and avatar identify it in conversations.</p>
        <p class={ui.muted}>Each Agent has one Slack connection in this Environment. Invite the app to the channels where you want to use it.</p>
        <div class={ui.modalActions}><button class={ui.button} disabled={busy()} onClick={() => void run(() => beginAgentSlackConnection(address))}>Connect Slack</button></div>
      </>}>{(connection) => <>
        <Show when={connection.status === "setup_incomplete"} fallback={<>
          <p><strong>{connection.app_name || props.agentName}</strong> in <strong>{connection.workspace_name || connection.team_id}</strong></p>
          <p class={ui.muted}>App {connection.app_id} · Workspace {connection.team_id}</p>
          <Show when={connection.status === "connected"}><p>Connected. Invite this app to a channel and mention it to start a conversation.</p></Show>
          <Show when={connection.status === "reauthorization_required"}><p class={ui.error}>Slack authorization needs repair. Reauthorize this same app and workspace to continue. Earlier unsent output will not be backfilled.</p></Show>
        </>}>
          <p>Create a dedicated Slack app for <strong>{props.agentName}</strong>, then save its credentials here.</p>
          <p class={ui.muted}>The app’s Basic Information page contains these credentials. Slack can verify its event URL after they are saved; retry URL verification in Slack if needed.</p>
          <Show when={state.data?.create_app_url}><a class={ui.secondaryButton} href={state.data!.create_app_url} target="_blank" rel="noopener noreferrer">Create app in Slack</a></Show>
          <AppIconPicker seed={iconSeed} agentName={props.agentName} />
          <details><summary class="cursor-pointer">App manifest</summary><pre class="max-h-48 overflow-auto whitespace-pre-wrap text-xs">{JSON.stringify(state.data?.manifest, null, 2)}</pre></details>
        </Show>
        <Show when={connection.status === "setup_incomplete" || connection.status === "reauthorization_required"}>
          <CredentialForm connection={connection} busy={busy()} save={(credentials) => run(() => saveSlackAppCredentials(address, connection.id, credentials))} />
        </Show>
        <Show when={connection.status === "connected"}><details><summary class="cursor-pointer">Download app icon</summary><AppIconPicker seed={iconSeed} agentName={props.agentName} /></details></Show>
        <Show when={connection.status === "connected"}><details><summary class="cursor-pointer">Update app credentials</summary><CredentialForm connection={connection} busy={busy()} save={(credentials) => run(() => saveSlackAppCredentials(address, connection.id, credentials))} /></details></Show>
        <Show when={connection.credentials_configured}>
          <button class={ui.button} disabled={busy()} onClick={() => void authorize(connection.id)}>{connection.installation_id ? "Reauthorize in Slack" : "Install app in Slack"}</button>
        </Show>
        <Show when={confirmDisconnect()} fallback={<button class={ui.dangerOutlineButton} disabled={busy()} onClick={() => setConfirmDisconnect(true)}>{connection.installation_id ? "Disconnect" : "Discard setup"}</button>}>
          <p>{connection.installation_id ? "Disconnecting stops Slack input, answers and output in existing threads. Work can continue through the CLI or API. Reconnecting creates a new connection; existing threads stay disconnected and triggers require re-promotion." : "Discard this setup to release the Agent’s connection slot. The app in Slack is not deleted."}</p>
          <div class={ui.modalActions}><button class={ui.secondaryButton} disabled={busy()} onClick={() => setConfirmDisconnect(false)}>Keep connection</button><button class={ui.dangerOutlineButton} disabled={busy()} onClick={() => void run(() => disconnectAgentSlackConnection(address, connection.id))}>{connection.installation_id ? "Disconnect Slack" : "Discard setup"}</button></div>
        </Show>
      </>}</Show>
    </Show>
  </Dialog>;
}

function CredentialForm(props: { connection: AgentSlackConnection; busy: boolean; save: (credentials: { client_id: string; client_secret: string; signing_secret: string }) => Promise<void> }) {
  const [client, setClient] = createSignal(props.connection.client_id || "");
  const [secret, setSecret] = createSignal("");
  const [signing, setSigning] = createSignal("");
  return <form class="flex flex-col gap-3" onSubmit={(event) => { event.preventDefault(); void props.save({ client_id: client().trim(), client_secret: secret().trim(), signing_secret: signing().trim() }).then(() => { setSecret(""); setSigning(""); }); }}>
    <label class="flex flex-col gap-1"><span>Client ID</span><input class={ui.input} required value={client()} readOnly={props.connection.credentials_configured} disabled={props.busy} autocomplete="off" onInput={(event) => setClient(event.currentTarget.value)} /></label>
    <label class="flex flex-col gap-1"><span>Client secret</span><input class={ui.input} type="password" required value={secret()} disabled={props.busy} autocomplete="new-password" onInput={(event) => setSecret(event.currentTarget.value)} /></label>
    <label class="flex flex-col gap-1"><span>Signing secret</span><input class={ui.input} type="password" required value={signing()} disabled={props.busy} autocomplete="new-password" onInput={(event) => setSigning(event.currentTarget.value)} /></label>
    <button class={ui.secondaryButton} type="submit" disabled={props.busy}>Save app credentials</button>
  </form>;
}
