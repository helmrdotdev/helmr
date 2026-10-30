import { createQuery } from "@tanstack/solid-query";
import { createSignal, onMount, Show } from "solid-js";
import { postJson, request } from "../lib/api";
import { getAuthConfig } from "../lib/auth";
import { errorMessage } from "../lib/error";
import { registerPasskey } from "../lib/passkey";
import { Field, SecretInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { DetailLayout, KeyValue, KeyValueList, Panel } from "../ui/Panel";
import { DeniedState } from "../ui/QueryState";
import { Steps } from "../ui/Steps";
import { ui } from "../ui/styles";

// A redeemed repair survives a reload, so the holder can register while the
// operator keeps the page open.
const REPAIR_KEY = "helmr.account-repair";

// Supervised repair of an ordinary account on a self-hosted deployment: the
// operator redeems a deployment-issued capability, both check the account, and
// the holder registers a new key with their own authenticator.
export function AccountRepair() {
  const config = createQuery(() => ({ queryKey: ["auth-config"], queryFn: getAuthConfig }));
  const [capability, setCapability] = createSignal("");
  const [repair, setRepairID] = createSignal(sessionStorage.getItem(REPAIR_KEY) ?? "");
  const [target, setTarget] = createSignal("");
  const [checked, setChecked] = createSignal(false);
  const [restored, setRestored] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const setRepair = (id: string) => { setRepairID(id); if (id) sessionStorage.setItem(REPAIR_KEY, id); else sessionStorage.removeItem(REPAIR_KEY); };
  const run = async (fn: () => Promise<void>) => {
    setBusy(true); setError("");
    try { await fn(); } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); }
  };
  onMount(() => {
    if (repair()) void run(async () => { setTarget((await request<{ target: string }>(`/admin/api/v1/account-repairs/${repair()}`)).target); });
  });
  const startOver = () => { setRepair(""); setTarget(""); setChecked(false); setRestored(false); setError(""); };
  const redeem = () => run(async () => {
    const result = await postJson<{ capability: string }, { enrollment_id: string; target: string }>("/admin/api/v1/account-repairs/redeem", { capability: capability().trim() });
    setCapability(""); setRepair(result.enrollment_id); setTarget(result.target);
  });
  const complete = () => run(async () => {
    await registerPasskey(`/admin/api/v1/account-repairs/${repair()}/options`, `/admin/api/v1/account-repairs/${repair()}/complete`, "Restored account key");
    setRepair(""); setRestored(true);
  });
  const step = () => restored() ? 3 : checked() ? 2 : target() ? 1 : 0;
  const state = (index: number) => step() > index ? "done" as const : step() === index ? "current" as const : "upcoming" as const;
  return (
    <Page>
      <PageHeader title="Account repair" subtitle="Restore sign-in for an ordinary account while its holder is present. Grants no membership or operator rights." />
      <Show when={config.data} fallback={<Show when={config.isError}><Notice tone="danger">{errorMessage(config.error)}</Notice></Show>}>
        {current => (
          <Show when={current().deployment_mode === "self-hosted"} fallback={<DeniedState message="Account repair is available on self-hosted deployments." />}>
            <DetailLayout rail={
              <KeyValueList title="What repair does">
                <KeyValue label="Restores">Sign-in with a new key</KeyValue>
                <KeyValue label="Revokes">Existing browser, CLI and Slack access</KeyValue>
                <KeyValue label="Keeps">Sign-in methods and memberships</KeyValue>
                <KeyValue label="Recorded in">Audit events</KeyValue>
              </KeyValueList>
            }>
              <Show when={restored()}>
                <Notice tone="success">Account access restored. Existing access is revoked. The account holder can now sign in normally with their key.</Notice>
              </Show>
              <Panel title="Supervised repair" actions={<Show when={repair() || restored()}><button type="button" class={ui.compactButton} disabled={busy()} onClick={startOver}>Start over</button></Show>}>
                <Steps label="Supervised repair" steps={[
                  {
                    title: "Enter the repair capability",
                    state: state(0),
                    description: "Issued by the deployment for one account.",
                    content: <form class="flex flex-wrap items-end gap-2" onSubmit={e => { e.preventDefault(); void redeem(); }}>
                      <Field label="Account repair capability" flush class="w-80 max-w-full"><SecretInput value={capability()} onInput={e => setCapability(e.currentTarget.value)} autocomplete="off" /></Field>
                      <button type="submit" class={ui.button} disabled={busy() || !capability().trim()}>Review repair target</button>
                    </form>,
                  },
                  {
                    title: "Check the account",
                    state: state(1),
                    description: "Confirm with the holder that this is their account.",
                    content: <div class={ui.blockStack}>
                      <dl class="m-0"><KeyValue label="Account">{target()}</KeyValue></dl>
                      <div><button type="button" class={ui.button} onClick={() => setChecked(true)}>This is the right account</button></div>
                    </div>,
                  },
                  {
                    title: "Holder registers a new key",
                    state: state(2),
                    description: "The holder uses their own authenticator while you supervise. Existing access is revoked.",
                    content: <div><button type="button" class={ui.button} disabled={busy()} onClick={() => void complete()}>{busy() ? "Waiting for key…" : "Account holder: register your key and complete repair"}</button></div>,
                  },
                ]} />
                <Show when={error()}><Notice tone="danger">{error()}</Notice></Show>
              </Panel>
            </DetailLayout>
          </Show>
        )}
      </Show>
    </Page>
  );
}
