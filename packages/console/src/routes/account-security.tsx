import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { listSignInMethods, removeSignInMethod, revokeAllAccess, revokeOtherAccess, startGitHubLink, type SignInMethod } from "../lib/account";
import { getAuthConfig } from "../lib/auth";
import { errorMessage, loadError } from "../lib/error";
import { useMe } from "../lib/me";
import { registerPasskey } from "../lib/passkey";
import { DataTable, RowActions, TableCount } from "../ui/DataTable";
import { ConfirmDialog, FormDialog } from "../ui/Dialog";
import { Field, TextInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { PageHeader } from "../ui/PageHeader";
import { DangerZone } from "../ui/Panel";
import { QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { TableSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";
import { Tag } from "../ui/Tag";

const COLUMNS = ["Sign-in method", "Type", "Added", "Last used", { label: "Actions", srOnly: true }] as const;
const METHOD_NAMES: Record<string, string> = { passkey: "passkey", email: "email", github: "GitHub" };

// How the account signs in, and signing out other sessions or everywhere.
// Removing a method needs a recent sign-in, which the confirmation dialog asks
// for, and leaves existing sessions signed in.
export function AccountSecurity() {
  const me = useMe();
  const cache = useQueryClient();
  const methods = createQuery(() => ({ queryKey: ["account", "sign-in-methods"], queryFn: listSignInMethods }));
  const config = createQuery(() => ({ queryKey: ["auth-config"], queryFn: getAuthConfig }));
  const [adding, setAdding] = createSignal(false);
  const [removing, setRemoving] = createSignal<SignInMethod>();
  const [revoking, setRevoking] = createSignal(false);
  const [revokingOthers, setRevokingOthers] = createSignal(false);
  const [linking, setLinking] = createSignal(false);
  const [notice, setNotice] = createSignal("");
  const [error, setError] = createSignal("");
  const refresh = () => cache.invalidateQueries({ queryKey: ["account", "sign-in-methods"] });
  const linkGitHub = async () => {
    setLinking(true); setError("");
    try { window.location.assign((await startGitHubLink()).redirect_url); }
    catch (cause) { setError(errorMessage(cause)); setLinking(false); }
  };
  return <>
    <PageHeader title="Sign-in and security" actions={<div class={ui.buttonGroup}>
      <Show when={config.data?.methods.includes("github")}>
        <button type="button" class={ui.secondaryButton} disabled={linking()} onClick={() => void linkGitHub()}>{linking() ? "Linking…" : "Link GitHub"}</button>
      </Show>
      <button type="button" class={ui.button} onClick={() => { setNotice(""); setAdding(true); }}>Add passkey</button>
    </div>} />
    <div class={ui.sectionStack}>
      <Show when={notice()}><Notice tone="success" onDismiss={() => setNotice("")}>{notice()}</Notice></Show>
      <Show when={error()}><Notice tone="danger" onDismiss={() => setError("")}>{error()}</Notice></Show>
      <QueryState query={methods} pending={<TableSkeleton label="Loading sign-in methods" columns={COLUMNS} />} error={cause => loadError(cause, "Could not load your sign-in methods.")}>
        {data => (
          <DataTable columns={COLUMNS} minWidth="min-w-150" footer={<TableCount count={data().credentials.length} noun={{ one: "sign-in method", other: "sign-in methods" }} />}>
            <For each={data().credentials}>
              {method => (
                <tr>
                  <td class="font-mono text-chrome!">{method.label}</td>
                  <td><Tag>{METHOD_NAMES[method.method] ?? method.method}</Tag></td>
                  <td><RelativeTime value={method.created_at} /></td>
                  <td><RelativeTime value={method.last_used_at} /></td>
                  <RowActions label={`Actions for ${method.label}`} items={[{ label: "Remove", tone: "danger", onSelect: () => { setNotice(""); setRemoving(method); } }]} />
                </tr>
              )}
            </For>
          </DataTable>
        )}
      </QueryState>
      <DangerZone title="Sign out other sessions" action={<button type="button" class={ui.dangerOutlineButton} onClick={() => { setNotice(""); setRevokingOthers(true); }}>Sign out other sessions…</button>}>
        Signs out every other browser and CLI and revokes Slack access issued for this account. This browser stays signed in.
      </DangerZone>
      <DangerZone title="Sign out everywhere" action={<button type="button" class={ui.dangerOutlineButton} onClick={() => setRevoking(true)}>Sign out everywhere…</button>}>
        Signs out every browser and CLI and revokes Slack access issued for this account. Accepted work continues; stop it from its own controls. Your sign-in methods and memberships stay. <Show when={me.data?.user_id}>{user => <a class={ui.monoLink} href={`/sessions?user_id=${encodeURIComponent(user())}`}>View your sessions</a>}</Show>
      </DangerZone>
    </div>
    <Show when={adding()}><AddPasskeyDialog labels={methods.data?.credentials.map(method => method.label) ?? []} onClose={() => setAdding(false)} onAdded={async label => { await refresh(); setNotice(`Passkey “${label}” added.`); }} /></Show>
    <Show when={removing()}>{method => (
      <ConfirmDialog title="Remove sign-in method" confirmLabel="Remove method" busyLabel="Removing…" tone="danger" target={method().label} onClose={() => setRemoving(undefined)} onConfirm={async () => { await removeSignInMethod(method().id); await refresh(); setNotice("Sign-in method removed."); }}>
        You can no longer sign in with this method. Browsers and CLI logins already signed in stay signed in; use Sign out other sessions to end them.
      </ConfirmDialog>
    )}</Show>
    <Show when={revokingOthers()}>
      <ConfirmDialog title="Sign out other sessions" confirmLabel="Sign out other sessions" busyLabel="Signing out…" tone="danger" onClose={() => setRevokingOthers(false)} onConfirm={async () => { await revokeOtherAccess(); setNotice("Other sessions signed out."); }}>
        Every other browser and CLI login for this account signs out, and its Slack access is revoked. This browser stays signed in.
      </ConfirmDialog>
    </Show>
    <Show when={revoking()}>
      <ConfirmDialog title="Sign out everywhere" confirmLabel="Sign out everywhere" busyLabel="Signing out…" tone="danger" onClose={() => setRevoking(false)} onConfirm={async () => { await revokeAllAccess(); window.location.assign("/login"); }}>
        Every browser, including this one, and every CLI login for this account signs out, and its Slack access is revoked.
      </ConfirmDialog>
    </Show>
  </>;
}

// The name starts as the next free "Passkey N", so adding one needs no typing.
function nextPasskeyName(labels: string[]) {
  const taken = new Set(labels);
  let n = 1;
  while (taken.has(`Passkey ${n}`)) n++;
  return `Passkey ${n}`;
}

function AddPasskeyDialog(props: { labels: string[]; onClose: () => void; onAdded: (label: string) => Promise<void> }) {
  const [label, setLabel] = createSignal(nextPasskeyName(props.labels));
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const submit = async () => {
    setBusy(true); setError("");
    try {
      await registerPasskey("/api/account/passkeys/options", "/api/account/passkeys/complete", label().trim());
      await props.onAdded(label().trim());
      props.onClose();
    } catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };
  return (
    <FormDialog title="Add passkey" intro="Your browser asks you to create the passkey on this device or a security key." submitLabel="Add passkey" busyLabel="Waiting for passkey…" busy={busy()} disabled={!label().trim()} error={error()} onSubmit={() => void submit()} onClose={props.onClose}>
      <Field label="Passkey name" hint="A name that tells you which device it is.">
        <TextInput value={label()} onInput={e => setLabel(e.currentTarget.value)} required maxlength={200} />
      </Field>
    </FormDialog>
  );
}
