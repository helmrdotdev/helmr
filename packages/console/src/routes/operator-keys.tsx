import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, onMount, Show } from "solid-js";
import { del, postJson, request } from "../lib/api";
import { errorMessage, loadError } from "../lib/error";
import { assertPasskey, registerPasskey } from "../lib/passkey";
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
import { Steps } from "../ui/Steps";
import { ui } from "../ui/styles";

type OperatorKey = { id: string; label: string; created_at: string; last_used_at?: string | null };

const COLUMNS = ["Key", "Added", "Last used", { label: "Actions", srOnly: true }] as const;
// An enrollment in progress survives a reload, so a key registered but not yet
// verified can be finished.
const ENROLLMENT_KEY = "helmr.operator-add-key";
// The server keeps the name sent when the key is verified, so it is remembered too.
const LABEL_KEY = "helmr.operator-add-key.label";

export function OperatorKeys() {
  const cache = useQueryClient();
  const keys = createQuery(() => ({ queryKey: ["operator", "keys"], queryFn: () => request<{ credentials: OperatorKey[] }>("/admin/api/v1/credentials", { redirectOnUnauthorized: false }) }));
  const [adding, setAdding] = createSignal<{ enrollment: string; registered: boolean }>();
  const [removing, setRemoving] = createSignal<OperatorKey>();
  const [notice, setNotice] = createSignal("");
  const [error, setError] = createSignal("");
  const refresh = () => cache.invalidateQueries({ queryKey: ["operator", "keys"] });
  // Adding resumes an enrollment left registered but unverified, whether the
  // page was reloaded or the panel closed.
  const openAdding = async (fresh: boolean) => {
    setNotice(""); setError("");
    const enrollment = sessionStorage.getItem(ENROLLMENT_KEY);
    if (!enrollment) { if (fresh) setAdding({ enrollment: "", registered: false }); return; }
    try { setAdding({ enrollment, registered: (await request<{ stage: string }>(`/admin/api/v1/enrollment/${enrollment}`)).stage === "assert" }); }
    catch (cause) { sessionStorage.removeItem(ENROLLMENT_KEY); setError(errorMessage(cause)); }
  };
  onMount(() => void openAdding(false));
  return (
    <Page>
      <PageHeader
        title="Operator keys"
        subtitle="Keys that sign in to deployment administration. They are separate from ordinary accounts."
        actions={<Show when={!adding()}><button type="button" class={ui.button} onClick={() => void openAdding(true)}>Add operator key</button></Show>}
      />
      <div class={ui.sectionStack}>
        <Show when={notice()}><Notice tone="success" onDismiss={() => setNotice("")}>{notice()}</Notice></Show>
        <Show when={error()}><Notice tone="danger" onDismiss={() => setError("")}>{error()}</Notice></Show>
        <Show when={adding()} keyed>
          {start => <AddOperatorKey enrollment={start.enrollment} registered={start.registered} onClose={() => setAdding(undefined)} onAdded={async () => { setAdding(undefined); await refresh(); setNotice("Operator key added and tested."); }} />}
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

// Adding a key is two steps on the page: register it with the authenticator,
// then sign in with it once, so a key that cannot sign in is never kept.
function AddOperatorKey(props: { enrollment: string; registered: boolean; onClose: () => void; onAdded: () => Promise<void> }) {
  const [label, setLabel] = createSignal(sessionStorage.getItem(LABEL_KEY) ?? "Operator key");
  const [enrollment, setEnrollment] = createSignal(props.enrollment);
  const [registered, setRegistered] = createSignal(props.registered);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const run = async (fn: () => Promise<void>) => {
    setBusy(true); setError("");
    try { await fn(); } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); }
  };
  const register = () => run(async () => {
    let id = enrollment();
    if (!id) {
      id = (await postJson<{}, { enrollment_id: string }>("/admin/api/v1/credentials/enroll", {})).enrollment_id;
      setEnrollment(id); sessionStorage.setItem(ENROLLMENT_KEY, id);
    }
    await registerPasskey(`/admin/api/v1/enrollment/${id}/options`, `/admin/api/v1/enrollment/${id}/complete`, label().trim());
    sessionStorage.setItem(LABEL_KEY, label().trim());
    setRegistered(true);
  });
  const verify = () => run(async () => {
    await assertPasskey(`/admin/api/v1/enrollment/${enrollment()}/assert/options`, `/admin/api/v1/enrollment/${enrollment()}/assert/verify`, { label: label().trim() });
    sessionStorage.removeItem(ENROLLMENT_KEY); sessionStorage.removeItem(LABEL_KEY);
    await props.onAdded();
  });
  const startOver = () => { sessionStorage.removeItem(ENROLLMENT_KEY); sessionStorage.removeItem(LABEL_KEY); setEnrollment(""); setRegistered(false); setError(""); };
  return (
    <Panel title="Add operator key" actions={<button type="button" class={ui.compactButton} disabled={busy()} onClick={props.onClose}>Close</button>}>
      <Steps label="Add operator key" steps={[
        {
          title: "Register the key",
          state: registered() ? "done" : "current",
          description: "Your browser asks you to create the key on this device or a security key.",
          content: <form class="flex flex-wrap items-end gap-2" onSubmit={e => { e.preventDefault(); void register(); }}>
            <Field label="Key name" flush class="w-60 max-w-full"><TextInput value={label()} onInput={e => setLabel(e.currentTarget.value)} required maxlength={200} disabled={busy()} /></Field>
            <button type="submit" class={ui.button} disabled={busy() || !label().trim()}>{busy() ? "Waiting for key…" : "Register key"}</button>
          </form>,
        },
        {
          title: "Sign in with the new key",
          state: registered() ? "current" : "upcoming",
          description: <span>Proves the key works before it is kept as <span class="font-mono text-console-text">{label()}</span>.</span>,
          content: <div class={ui.buttonGroup}>
            <button type="button" class={ui.button} disabled={busy()} onClick={() => void verify()}>{busy() ? "Waiting for key…" : "Verify key"}</button>
            <button type="button" class={ui.secondaryButton} disabled={busy()} onClick={startOver}>Start over</button>
          </div>,
        },
      ]} />
      <Show when={error()}><Notice tone="danger">{error()}</Notice></Show>
    </Panel>
  );
}
