import { A } from "@solidjs/router";
import { createSignal, For, Show, type JSX } from "solid-js";
import { computerHref } from "../computers/navigation";
import { deploymentHref } from "../deployments/navigation";
import type { AdminAuditCheck, AnyAuditEvent as AuditEvent, AuditRestrictedSection } from "../../lib/audit";
import { actionLabel, actorOf, checkFields, checkTitle, signInFields, targetLabel, targetNoun, type AuditField, type AuditNames } from "../../lib/auditText";
import { sessionConsolePath } from "../../lib/sessions";
import { formatID } from "../../ui/id";
import { JsonView } from "../../ui/PayloadView";
import { RelativeTime } from "../../ui/RelativeTime";
import { StatusBadge } from "../../ui/StatusBadge";
import { TabPanel, Tabs } from "../../ui/Tabs";
import { useClipboard } from "../../ui/useClipboard";
import { cx, ui } from "../../ui/styles";

type Tab = "event" | "authorization" | "json";

const roleHref = (id: string) => `/settings/roles/${encodeURIComponent(id)}`;
const LINK_BUTTON = cx(ui.monoLink, "border-0 bg-transparent p-0 font-sans text-control");
const DAY = 86_400_000;

// A group heading and its labelled values, one per line.
function Group(props: { title: string; children: JSX.Element }) {
  return (
    <section>
      <h4 class="m-0 flex h-7 items-center [section:first-child>&]:border-t-0 border-y border-console-border-soft bg-console-bg-panel px-3 text-control font-medium text-console-text">{props.title}</h4>
      <dl class="m-0">{props.children}</dl>
    </section>
  );
}

function Row(props: { label: string; children: JSX.Element; id?: string | undefined; sub?: string | undefined }) {
  return (
    <div class="grid min-h-6.5 grid-cols-[180px_minmax(0,1fr)] items-baseline gap-3 border-b border-console-border-soft px-3 py-1 last:border-b-0 max-sm:grid-cols-1 max-sm:gap-0.5">
      <dt class="text-console-muted">{props.label}</dt>
      <dd class="m-0 min-w-0 break-words text-console-text">
        {props.children}
        <Show when={props.sub}><span class="ml-2 text-console-muted">{props.sub}</span></Show>
        <Show when={props.id}><span class="ml-2 font-mono text-micro text-console-subtle">{props.id}</span></Show>
      </dd>
    </div>
  );
}

function FieldRow(props: { field: AuditField }) {
  const value = () => {
    const f = props.field;
    const text = f.tone ? `${f.tone === "passed" ? "✓" : "✕"} ${f.value}` : f.value;
    const styled = <span class={cx(f.code && "font-mono text-chrome", f.muted && "text-console-muted", f.tone === "passed" && "text-console-success", f.tone === "failed" && "text-console-danger")}>{text}</span>;
    return f.href ? <A href={f.href} class={cx(ui.monoLink, !f.code && "font-sans text-control")}>{f.value}</A> : styled;
  };
  return <Row label={props.field.label} id={props.field.id} sub={props.field.sub}>{value()}</Row>;
}

function Fields(props: { children: JSX.Element; footer?: JSX.Element }) {
  return (
    <div class="border border-console-border bg-console-surface text-control">
      {props.children}
      <Show when={props.footer}><div class="flex flex-wrap items-center gap-3.5 border-t border-console-border-soft px-3 py-1.5 text-chrome text-console-muted">{props.footer}</div></Show>
    </div>
  );
}

// One event's recorded fields under plain labels, beneath its row: what
// happened, who acted and how they signed in, what it acted on and what it
// relates to; the authority checks Helmr recorded; and the record as JSON.
export function AuditEventDetails(props: {
  event: AuditEvent;
  names: AuditNames;
  memberHref: (userID: string) => string | undefined;
  rolesWith: (permission: string) => { id: string; name: string }[];
  onSelectOperation: () => void;
  onSelectCause: () => void;
  link: string;
}) {
  const [tab, setTab] = createSignal<Tab>("event");
  const clipboard = useClipboard({ copied: "Copied", failed: "Could not copy" });
  const e = () => props.event;
  const actor = () => actorOf(e());
  const restricted = (section: AuditRestrictedSection) => { const event = e(); return "restricted" in event && event.restricted.includes(section); };
  const cause = () => { const event = e(); return "cause_event_id" in event ? event.cause_event_id : null; };
  const runtime = () => { const event = e(); return "runtime_generation" in event && event.runtime_generation ? `Generation ${event.runtime_generation}${event.process_epoch ? `, process ${event.process_epoch}` : ""}` : undefined; };
  const installationAuthority = () => { const auth = e().authorization; return "installation_authority" in auth && auth.installation_authority; };
  const checks = (): AdminAuditCheck[] => e().authorization.checks ?? [];
  const failed = () => checks().filter((c) => c.result !== "passed").length;
  const tabsID = () => `audit-${e().id}`;
  const recordedLater = () => Date.parse(e().recorded_at) - Date.parse(e().occurred_at);
  const target = () => targetLabel(e(), props.names);
  const targetHref = () => {
    const id = e().target_id;
    if (!id) return undefined;
    if (e().target_kind === "session") return sessionConsolePath(id);
    if (e().target_kind === "computer") return computerHref(id);
    if (e().target_kind === "role") return roleHref(id);
    return undefined;
  };
  const missing = () => {
    const check = checks().find((c) => c.type === "permission" && c.result === "failed" && c.permission);
    return check?.permission ? { permission: check.permission, roles: props.rolesWith(check.permission) } : undefined;
  };
  const details = () => (e().details && typeof e().details === "object" ? Object.entries(e().details as Record<string, unknown>).filter(([, value]) => value !== null && value !== undefined && value !== "") : []);

  return (
    <div class="flex flex-col gap-2.5 border-b border-console-border bg-console-accent-soft/40 py-2.5 pr-6 pl-10 max-sm:pl-3">
      <clipboard.LiveRegion />
      <Show when={"restricted" in e() && (e() as import("../../lib/audit").AuditEvent).restricted.length}><p class="m-0 text-chrome text-console-muted">Some installation details are restricted in this view. An installation administrator can correlate the full record by event ID.</p></Show>
      <div class="flex items-center gap-3 border-b border-console-border">
        <Tabs<Tab> id={tabsID()} label="Event details" class="mb-0! min-w-0 flex-1 border-b-0!" selected={tab()} onSelect={setTab} items={[
          { value: "event", label: "Event" },
          { value: "authorization", label: "Authorization", count: checks().length },
          { value: "json", label: "JSON" },
        ]} />
        <div class="flex shrink-0 items-center gap-3.5">
          <Show when={tab() !== "json"}><button type="button" class={ui.payloadAction} onClick={() => void clipboard.copy(JSON.stringify(e(), null, 2))}>Copy JSON</button></Show>
          <button type="button" class={ui.payloadAction} onClick={() => void clipboard.copy(props.link)}>Copy link</button>
        </div>
      </div>

      <TabPanel tabs={tabsID()} value="event" selected={tab()}>
        <Fields footer={<><span>Event <span class="font-mono">{formatID(e().id)}</span></span><Show when={actor().kind === "member" && actor().id && props.memberHref(actor().id!)}>{(href) => <A href={href()} class={cx(ui.monoLink, "font-sans")}>Review {actor().name}'s access</A>}</Show></>}>
          <Group title="Event">
            <Row label="Action" id={e().action}>{actionLabel(e().action)}</Row>
            <Row label="Outcome"><StatusBadge resource="audit_outcome" status={e().outcome as "succeeded" | "denied"} /></Row>
            <Row label="When"><time datetime={e().occurred_at}>{new Date(e().occurred_at).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "medium" })}</time><span class="ml-2 text-console-muted"><RelativeTime value={e().occurred_at} /></span></Row>
            <Show when={recordedLater() > 0}><Row label="Recorded">{recordedLater() < 1000 ? `${recordedLater()} ms later` : `${Math.round(recordedLater() / 1000)} s later`}</Row></Show>
          </Group>
          <Group title="Who">
            <Show when={actor().kind === "member"}>
              <Row label="Member" id={actor().id ? formatID(actor().id!) : undefined} sub={actor().detail}>
                <Show when={actor().id && props.memberHref(actor().id!)} fallback={actor().name}>{(href) => <A href={href()} class={cx(ui.monoLink, "font-sans text-control")}>{actor().name}</A>}</Show>
              </Row>
              <Show when={e().actor_identity?.source}><Row label="Name from">{e().actor_identity!.source === "console" ? "Console profile" : e().actor_identity!.source}</Row></Show>
            </Show>
            <Show when={actor().kind === "api_key"}><Row label="API key" id={formatID(actor().id!)}>{actor().name}</Row></Show>
            <Show when={actor().kind === "session"}>
              <Row label="Agent session"><A href={sessionConsolePath(actor().id!)} class={ui.monoLink}>{formatID(actor().id!)}</A></Row>
              <Show when={runtime() || restricted("runtime")}><Row label="Runtime">{restricted("runtime") ? "Restricted" : runtime()}</Row></Show>
            </Show>
            <Show when={actor().kind === "worker_host"}><Row label="Worker host" id={formatID(actor().id!)}>{actor().name}</Row></Show>
            <Show when={actor().kind === "helmr"}><Row label="Helmr">{actor().detail?.replace(/^./, (c) => c.toUpperCase())}</Row></Show>
            <Show when={actor().kind === "platform_service"}><Row label="Actor">Platform service</Row></Show>
            <Show when={actor().kind === "unidentified"}><Row label="Actor"><span class="text-console-muted">Not identified</span></Row></Show>
          </Group>
          <Group title="Sign-in"><For each={signInFields(e())}>{(field) => <FieldRow field={field} />}</For></Group>
          <Group title="Acted on">
            <Show when={target()}>
              <Row label={targetNoun(e().target_kind)} id={e().target_id && !target()!.endsWith(formatID(e().target_id!)) ? formatID(e().target_id!) : undefined}>
                <Show when={targetHref()} fallback={target()}>{(href) => <A href={href()} class={cx(ui.monoLink, "font-sans text-control")}>{target()}</A>}</Show>
              </Row>
            </Show>
            <Show when={e().target_user_id && e().target_user_id !== e().target_id}><Row label="Person" id={formatID(e().target_user_id!)}>{props.names.get(e().target_user_id!) ?? "A member"}</Row></Show>
            <Show when={e().session_id && e().target_id !== e().session_id}><Row label="Session"><A href={sessionConsolePath(e().session_id!)} class={ui.monoLink}>{formatID(e().session_id!)}</A></Row></Show>
            <Show when={e().turn_id && e().session_id}><Row label="Turn" id={formatID(e().turn_id!)}><A href={`${sessionConsolePath(e().session_id!)}#turn-${e().turn_id}`} class={ui.monoLink}>Open the turn</A></Row></Show>
            <Show when={e().computer_id && e().target_id !== e().computer_id}><Row label="Computer"><A href={computerHref(e().computer_id!)} class={ui.monoLink}>{formatID(e().computer_id!)}</A></Row></Show>
            <Show when={e().deployment_id}><Row label="Deployment"><A href={deploymentHref(e().deployment_id!)} class={ui.monoLink}>{formatID(e().deployment_id!)}</A></Row></Show>
            <Show when={e().slack_user_id}><Row label="Slack user" sub={e().slack_team_id ? `workspace ${e().slack_team_id}` : undefined}><span class="font-mono text-chrome">{e().slack_user_id}</span></Row></Show>
            <Row label="Environment" id={e().environment_id ? formatID(e().environment_id!) : undefined}>{e().environment_id ? props.names.get(e().environment_id!) ?? "An environment" : <span class="text-console-muted">None; organization-wide</span>}</Row>
          </Group>
          <Show when={details().length}>
            <Group title="Details">
              <For each={details()}>{([key, value]) => <Row label={key.replaceAll("_", " ").replace(/^./, (c) => c.toUpperCase())}><span class={typeof value === "object" ? "font-mono text-chrome" : undefined}>{typeof value === "object" ? JSON.stringify(value) : String(value)}</span></Row>}</For>
            </Group>
          </Show>
          <Group title="Related">
            <Row label="Operation" id={formatID(e().operation_id)}>
              <button type="button" class={LINK_BUTTON} onClick={() => props.onSelectOperation()}>Show every event it recorded</button>
            </Row>
            <Row label="Caused by" id={cause() ? formatID(cause()!) : undefined}>
              <Show when={cause()} fallback={<span class="text-console-muted">{restricted("causality") ? "Restricted" : "No cause recorded"}</span>}>
                <button type="button" class={LINK_BUTTON} onClick={() => props.onSelectCause()}>Open the cause</button>
              </Show>
            </Row>
            <Row label="Kept until" sub={`${Math.round(e().retention_seconds * 1000 / DAY)} days`}>{new Date(e().expires_at).toLocaleDateString(undefined, { dateStyle: "medium" })}</Row>
          </Group>
        </Fields>
      </TabPanel>

      <TabPanel tabs={tabsID()} value="authorization" selected={tab()}>
        <Fields footer={missing() && missing()!.roles.length ? <span>Roles that include <span class="font-mono">{missing()!.permission}</span> today: <For each={missing()!.roles}>{(role, i) => <>{i() > 0 ? ", " : ""}<A href={roleHref(role.id)} class={cx(ui.monoLink, "font-sans")}>{role.name}</A></>}</For></span> : undefined}>
          <Group title="Decision">
            <Row label="Result"><StatusBadge resource="audit_decision" status={e().authorization.decision} /><Show when={e().authorization.decision === "not_evaluated"}><span class="ml-2 text-console-muted">No authority check applies to this event</span></Show></Row>
            <Show when={installationAuthority()}><Row label="Authority">Installation administrator</Row></Show>
            <Row label="Checks">{restricted("authorization") ? <span class="text-console-muted">{checks().length} visible; additional checks restricted</span> : checks().length === 0 ? <span class="text-console-muted">None recorded</span> : `${checks().length} recorded${failed() ? `, ${failed()} failed` : ", all passed"}`}</Row>
          </Group>
          <For each={checks()}>{(check, i) => (
            <Group title={`Check ${i() + 1} · ${checkTitle(check)}`}>
              <For each={checkFields(check, props.names, { session: sessionConsolePath, role: roleHref })}>{(field) => <FieldRow field={field} />}</For>
            </Group>
          )}</For>
        </Fields>
      </TabPanel>

      <TabPanel tabs={tabsID()} value="json" selected={tab()}>
        <JsonView value={e() as never} label="Audit event record" />
      </TabPanel>
    </div>
  );
}
