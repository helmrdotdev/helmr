import { createMemo, createSignal, For, Show } from "solid-js";
import { searchAuditEvents, type AuditBefore, type AuditFilter, type AuditPage } from "../lib/audit";
import { hasPermission } from "../lib/auth";
import { errorMessage } from "../lib/error";
import { useMe } from "../lib/me";
import { DataTable } from "../ui/DataTable";
import { Field, TextInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { PageHeader } from "../ui/PageHeader";
import { ui } from "../ui/styles";

const COLUMNS = ["Recorded", "Actor", "Action", "Outcome", "Target", "Evidence"] as const;
const localTime = (date: Date) => new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);

export function AuditEvents(props: { installation?: boolean }) {
  const me = useMe();
  const allowed = createMemo(() => props.installation ? me.data?.admin : hasPermission(me.data, "audit_events.read") || me.data?.permission_scopes?.some(g => g.permissions.includes("audit_events.read")));
  const [from, setFrom] = createSignal(localTime(new Date(Date.now() - 86_400_000)));
  const [to, setTo] = createSignal(localTime(new Date(Date.now() + 60_000)));
  const [action, setAction] = createSignal(""); const [actor, setActor] = createSignal("");
  const [target, setTarget] = createSignal(""); const [outcome, setOutcome] = createSignal("");
  const [environment, setEnvironment] = createSignal(""); const [organization, setOrganization] = createSignal("");
  const [filter, setFilter] = createSignal<AuditFilter>();
  const [page, setPage] = createSignal<AuditPage>();
  const [busy, setBusy] = createSignal(false); const [error, setError] = createSignal<string>();
  async function load(next: AuditFilter, before?: AuditBefore) {
    setBusy(true); setError(undefined); setPage(undefined);
    try { setPage(await searchAuditEvents(next, before, props.installation)); setFilter(next); }
    catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  }
  function submit(event: SubmitEvent) {
    event.preventDefault();
    const start = new Date(from()), end = new Date(to());
    if (!Number.isFinite(start.getTime()) || !Number.isFinite(end.getTime()) || end <= start || end.getTime() - start.getTime() > 31 * 86_400_000) {
      setError("Choose a start and end time no more than 31 days apart."); return;
    }
    void load({ from: start.toISOString(), to: end.toISOString(), action: action(), actor_id: actor(), target_id: target(), outcome: outcome(), environment_id: environment(), organization_id: props.installation ? organization() : "" });
  }
  return <>
    <PageHeader title={props.installation ? "Installation audit" : "Audit events"} subtitle="Investigate recorded security activity within your current access." />
    <Show when={allowed()} fallback={<Show when={!me.isPending}><Notice tone="info">Audit events read permission is required for an organization or environment.</Notice></Show>}>
      <form onSubmit={submit} class="my-4 grid grid-cols-2 gap-3 max-[720px]:grid-cols-1">
        <label class={ui.fieldLabel}>From (local time)<input class={ui.input} type="datetime-local" required value={from()} onInput={e => setFrom(e.currentTarget.value)} /></label>
        <label class={ui.fieldLabel}>To (local time)<input class={ui.input} type="datetime-local" required value={to()} onInput={e => setTo(e.currentTarget.value)} /></label>
        <Field label="Action"><TextInput placeholder="All actions" value={action()} onInput={e => setAction(e.currentTarget.value)} /></Field>
        <label class={ui.fieldLabel}>Outcome<select class={ui.input} value={outcome()} onChange={e => setOutcome(e.currentTarget.value)}><option value="">All outcomes</option><For each={["accepted", "succeeded", "denied", "failed", "cancelled", "unknown"]}>{value => <option value={value}>{value}</option>}</For></select></label>
        <Field label="Actor ID"><TextInput placeholder="Any actor" value={actor()} onInput={e => setActor(e.currentTarget.value)} /></Field>
        <Field label="Target ID"><TextInput placeholder="Any target" value={target()} onInput={e => setTarget(e.currentTarget.value)} /></Field>
        <Field label="Environment ID" hint="Leave empty to search all audit scopes available to you."><TextInput value={environment()} onInput={e => setEnvironment(e.currentTarget.value)} /></Field>
        <Show when={props.installation}><Field label="Organization ID" hint="An original organization ID can include retained history after closure."><TextInput value={organization()} onInput={e => setOrganization(e.currentTarget.value)} /></Field></Show>
        <div class="col-span-full"><button class={ui.button} type="submit" disabled={busy()}>{busy() ? "Searching…" : "Search events"}</button></div>
      </form>
      <Notice tone="info">Searches are recorded. Results exclude expired events. Environment history excludes Organization IAM changes; use Organization audit access to investigate changes to Roles, groups and membership. Pages can change as records arrive or expire; repeat a time range when investigating. Security metadata does not include conversation content or command output.</Notice>
      <Show when={error()}>{message => <Notice tone="danger">{message()}</Notice>}</Show>
      <Show when={page()}>{result => <>
        <DataTable columns={COLUMNS} minWidth="min-w-180">
          <Show when={result().events.length > 0} fallback={<tr><td colSpan={COLUMNS.length}>No events match this range, filters and current access.</td></tr>}>
            <For each={result().events}>{event => <tr class="[&>td]:align-top!">
              <td><time datetime={event.recorded_at}>{new Date(event.recorded_at).toLocaleString()}</time></td>
              <td>{event.actor_identity?.display_name || event.actor_component || event.actor_user_id || event.actor_api_key_id || event.actor_session_id || event.actor_worker_host_id || "Unidentified"}<Show when={event.actor_identity?.email}><div class="text-console-subtle">{event.actor_identity?.email}</div></Show></td>
              <td>{event.action}</td><td>{event.outcome}</td>
              <td>{event.target_kind}<div class="break-all font-mono text-chrome">{event.target_id || event.target_user_id || "—"}</div><Show when={event.target_resolution === "unresolved"}><span>Unresolved</span></Show></td>
              <td><details><summary class="cursor-pointer">View evidence</summary><dl class="my-2 space-y-2 text-control"><dt>Environment</dt><dd class="break-all">{event.environment_id || "Organization / installation"}</dd><dt>Occurred</dt><dd>{event.occurred_at}</dd><dt>Expires</dt><dd>{event.expires_at}</dd><dt>Authentication</dt><dd>{event.authentication.result}</dd><dt>Authorization</dt><dd>{event.authorization.decision}</dd><dt>Credential</dt><dd class="break-all">{event.authentication.credential_kind} {event.authentication.credential_id}</dd><dt>Source</dt><dd>{event.source_ip || "Unavailable"} · {event.channel}</dd><dt>Operation</dt><dd class="break-all">{event.operation_id}</dd></dl><pre class="max-h-96 max-w-xl overflow-auto whitespace-pre-wrap break-all text-control">{JSON.stringify(event, null, 2)}</pre></details></td>
            </tr>}</For>
          </Show>
        </DataTable>
        <div class="my-3 flex items-center gap-3"><span>{result().events.length} events on this page</span><Show when={result().next_before}>{before => <button type="button" class={ui.button} disabled={busy()} onClick={() => { const current = filter(); if (current) void load(current, before()); }}>Older events</button>}</Show></div>
      </>}</Show>
    </Show>
  </>;
}
