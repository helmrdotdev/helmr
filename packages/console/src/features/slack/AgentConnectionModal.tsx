import { createQuery } from "@tanstack/solid-query";
import { createSignal, onCleanup, Show } from "solid-js";
import { authorizeAgentSlackConnection, beginAgentSlackConnection, disconnectAgentSlackConnection, getAgentSlackConnection, saveSlackAppCredentials, type AgentSlackAddress, type AgentSlackPublication } from "../../lib/slack";
import { Modal } from "../../ui/Modal";
import { ui } from "../../ui/styles";

export function AgentConnectionModal(props: AgentSlackAddress & { onClose: () => void }) {
  const address = { projectID: props.projectID, environmentID: props.environmentID, agentName: props.agentName };
  const connection = createQuery(() => ({ queryKey: ["agent-slack", address.projectID, address.environmentID, address.agentName], queryFn: () => getAgentSlackConnection(address), retry: false, gcTime: 0, refetchOnWindowFocus: false }));
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<string>();
  const [confirmDisconnect, setConfirmDisconnect] = createSignal(false);
  let mounted = true;
  onCleanup(() => { mounted = false; });
  const run = async (action: () => Promise<unknown>) => {
    setBusy(true); setError(undefined);
    try { await action(); if (mounted) { setConfirmDisconnect(false); await connection.refetch(); } }
    catch (failure) { if (mounted) setError(failure instanceof Error ? failure.message : "The connection could not be updated."); }
    finally { if (mounted) setBusy(false); }
  };
  const authorize = (id: string) => run(async () => {
    const result = await authorizeAgentSlackConnection(address, id, window.location.pathname + window.location.search);
    if (mounted) window.location.assign(result.redirect_url);
  });
  return <Modal title={`Slack · ${props.agentName}`} onClose={props.onClose} closeDisabled={busy()}>
    <Show when={connection.isPending}><p class={ui.muted}>Loading connection...</p></Show>
    <Show when={connection.error}><p class={ui.error}>The connection could not be loaded. An organization owner or admin can manage it.</p></Show>
    <Show when={error()}><p class={ui.error} role="alert">{error()}</p></Show>
    <Show when={connection.data}>
      <Show when={connection.data?.publication} keyed fallback={<>
        <p>Connect this Agent to Slack with its own app. Its app name and avatar identify it in conversations.</p>
        <p class={ui.muted}>Each Agent has one Slack connection in this Environment. Invite the app to the channels where you want to use it.</p>
        <div class={ui.modalActions}><button class={ui.button} disabled={busy()} onClick={() => void run(() => beginAgentSlackConnection(address))}>Connect Slack</button></div>
      </>}>{(publication) => <>
        <Show when={publication.status === "setup_incomplete"} fallback={<>
          <p><strong>{publication.app_name || props.agentName}</strong> in <strong>{publication.workspace_name || publication.team_id}</strong></p>
          <p class={ui.muted}>App {publication.app_id} · Workspace {publication.team_id}</p>
          <Show when={publication.status === "connected"}><p>Connected. Invite this app to a channel and mention it to start a conversation.</p></Show>
          <Show when={publication.status === "reauthorization_required"}><p class={ui.error}>Slack authorization needs repair. Reauthorize this same app and workspace to continue. Earlier unsent output will not be backfilled.</p></Show>
        </>}>
          <p>Create a dedicated Slack app for <strong>{props.agentName}</strong>, then save its credentials here.</p>
          <p class={ui.muted}>The app’s Basic Information page contains these credentials. Slack can verify its event URL after they are saved; retry URL verification in Slack if needed.</p>
          <Show when={connection.data?.create_app_url}><a class={ui.secondaryButton} href={connection.data!.create_app_url} target="_blank" rel="noopener noreferrer">Create app in Slack</a></Show>
          <details><summary class="cursor-pointer">App manifest</summary><pre class="max-h-48 overflow-auto whitespace-pre-wrap text-xs">{JSON.stringify(connection.data?.manifest, null, 2)}</pre></details>
        </Show>
        <Show when={publication.status === "setup_incomplete" || publication.status === "reauthorization_required"}>
          <CredentialForm publication={publication} busy={busy()} save={(credentials) => run(() => saveSlackAppCredentials(address, publication.id, credentials))} />
        </Show>
        <Show when={publication.status === "connected"}><details><summary class="cursor-pointer">Update app credentials</summary><CredentialForm publication={publication} busy={busy()} save={(credentials) => run(() => saveSlackAppCredentials(address, publication.id, credentials))} /></details></Show>
        <Show when={publication.credentials_configured}>
          <button class={ui.button} disabled={busy()} onClick={() => void authorize(publication.id)}>{publication.installation_id ? "Reauthorize in Slack" : "Install app in Slack"}</button>
        </Show>
        <Show when={confirmDisconnect()} fallback={<button class={ui.dangerOutlineButton} disabled={busy()} onClick={() => setConfirmDisconnect(true)}>{publication.installation_id ? "Disconnect" : "Discard setup"}</button>}>
          <p>{publication.installation_id ? "Disconnecting stops Slack input, answers and output in existing threads. Work can continue through the CLI or API. Reconnecting creates a new connection; existing threads stay disconnected and schedules require re-promotion." : "Discard this setup to release the Agent’s connection slot. The app in Slack is not deleted."}</p>
          <div class={ui.modalActions}><button class={ui.secondaryButton} disabled={busy()} onClick={() => setConfirmDisconnect(false)}>Keep connection</button><button class={ui.dangerOutlineButton} disabled={busy()} onClick={() => void run(() => disconnectAgentSlackConnection(address, publication.id))}>{publication.installation_id ? "Disconnect Slack" : "Discard setup"}</button></div>
        </Show>
      </>}</Show>
    </Show>
  </Modal>;
}

function CredentialForm(props: { publication: AgentSlackPublication; busy: boolean; save: (credentials: { client_id: string; client_secret: string; signing_secret: string }) => Promise<void> }) {
  const [client, setClient] = createSignal(props.publication.client_id || "");
  const [secret, setSecret] = createSignal("");
  const [signing, setSigning] = createSignal("");
  return <form class="flex flex-col gap-3" onSubmit={(event) => { event.preventDefault(); void props.save({ client_id: client().trim(), client_secret: secret().trim(), signing_secret: signing().trim() }).then(() => { setSecret(""); setSigning(""); }); }}>
    <label class="flex flex-col gap-1"><span>Client ID</span><input class={ui.input} required value={client()} readOnly={props.publication.credentials_configured} disabled={props.busy} autocomplete="off" onInput={(event) => setClient(event.currentTarget.value)} /></label>
    <label class="flex flex-col gap-1"><span>Client secret</span><input class={ui.input} type="password" required value={secret()} disabled={props.busy} autocomplete="new-password" onInput={(event) => setSecret(event.currentTarget.value)} /></label>
    <label class="flex flex-col gap-1"><span>Signing secret</span><input class={ui.input} type="password" required value={signing()} disabled={props.busy} autocomplete="new-password" onInput={(event) => setSigning(event.currentTarget.value)} /></label>
    <button class={ui.secondaryButton} type="submit" disabled={props.busy}>Save app credentials</button>
  </form>;
}
