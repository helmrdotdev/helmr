import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { del, request } from "../lib/api";
import { errorMessage, loadError } from "../lib/error";
import { registerPasskey } from "../lib/passkey";
import { DataTable, RowActions, TableCount } from "../ui/DataTable";
import { ConfirmDialog } from "../ui/Dialog";
import { Field, TextInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { TableSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";

type OperatorKey = { id: string; label: string; created_at: string; last_used_at?: string | null };

const COLUMNS = ["Key", "Added", "Last used", { label: "Actions", srOnly: true }] as const;

export function OperatorKeys() {
  const cache = useQueryClient();
  const keys = createQuery(() => ({ queryKey: ["operator", "keys"], queryFn: () => request<{ credentials: OperatorKey[] }>("/admin/api/v1/credentials", { redirectOnUnauthorized: false }) }));
  const [adding, setAdding] = createSignal(false);
  const [removing, setRemoving] = createSignal<OperatorKey>();
  const [notice, setNotice] = createSignal("");
  const [error, setError] = createSignal("");
  const refresh = () => cache.invalidateQueries({ queryKey: ["operator", "keys"] });
  const openAdding = () => { setNotice(""); setError(""); setAdding(true); };
  return (
    <Page>
      <PageHeader
        title="Operator keys"
        subtitle="Keys that sign in to deployment administration. They are separate from ordinary accounts."
        actions={<Show when={!adding()}><button type="button" class={ui.button} onClick={openAdding}>Add operator key</button></Show>}
      />
      <div class={ui.sectionStack}>
        <Show when={notice()}><Notice tone="success" onDismiss={() => setNotice("")}>{notice()}</Notice></Show>
        <Show when={error()}><Notice tone="danger" onDismiss={() => setError("")}>{error()}</Notice></Show>
        <Show when={adding()}>
          <AddOperatorKey onClose={() => setAdding(false)} onAdded={async () => { setAdding(false); await refresh(); setNotice("Operator key added."); }} />
        </Show>
        <QueryState query={keys} pending={<TableSkeleton label="Loading operator keys" columns={COLUMNS} />} error={cause => loadError(cause, "Could not load operator keys.")}>
          {data => (
            <DataTable columns={COLUMNS} footer={<TableCount count={data().credentials.length} noun={{ one: "key", other: "keys" }} />}>
              <For each={data().credentials}>
                {key => (
                  <tr>
                    <td class="font-mono text-chrome!">{key.label}</td>
                    <td><RelativeTime value={key.created_at} /></td>
                    <td><RelativeTime value={key.last_used_at} /></td>
                    <RowActions label={`Actions for ${key.label}`} items={[{ label: "Remove", tone: "danger", onSelect: () => { setNotice(""); setRemoving(key); } }]} />
                  </tr>
                )}
              </For>
            </DataTable>
          )}
        </QueryState>
        <p class={ui.muted}>Keep at least two keys. Removing a key needs a confirmation with another key you keep.</p>
      </div>
      <Show when={removing()}>{key => (
        <ConfirmDialog title="Remove operator key" confirmLabel="Remove key" busyLabel="Removing…" tone="danger" target={key().label} onClose={() => setRemoving(undefined)} onConfirm={async () => { await del(`/admin/api/v1/credentials/${encodeURIComponent(key().id)}`); await refresh(); setNotice("Operator key removed."); }}>
          The key can no longer sign in to deployment administration. Confirm with another operator key you will keep first.
        </ConfirmDialog>
      )}</Show>
    </Page>
  );
}

// Adding a key registers it with the authenticator and keeps it under the
// name given here.
function AddOperatorKey(props: { onClose: () => void; onAdded: () => Promise<void> }) {
  const [label, setLabel] = createSignal("Operator key");
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const register = async () => {
    setBusy(true); setError("");
    try {
      await registerPasskey("/admin/api/v1/credentials/options", "/admin/api/v1/credentials/complete", label().trim());
      await props.onAdded();
    } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); }
  };
  return (
    <Panel title="Add operator key" actions={<button type="button" class={ui.compactButton} disabled={busy()} onClick={props.onClose}>Close</button>}>
      <form class="flex flex-wrap items-end gap-2" onSubmit={e => { e.preventDefault(); void register(); }}>
        <Field label="Key name" flush class="w-60 max-w-full"><TextInput value={label()} onInput={e => setLabel(e.currentTarget.value)} required maxlength={200} disabled={busy()} /></Field>
        <button type="submit" class={ui.button} disabled={busy() || !label().trim()}>{busy() ? "Waiting for key…" : "Register key"}</button>
      </form>
      <p class={ui.muted}>Your browser asks you to create the key on this device or a security key.</p>
      <Show when={error()}><Notice tone="danger">{error()}</Notice></Show>
    </Panel>
  );
}
