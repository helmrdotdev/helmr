import { useSearchParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { AUTH_MESSAGES, errorMessage, loadError } from "../lib/error";
import { getAuthConfig } from "../lib/auth";
import { useMe } from "../lib/me";
import {
  activateSSOConnection, createSSOConnection, deleteSSOConnection, getSSOStatus, listSSOMembers,
  testSSOConnection, unlinkSSOMember, updateSSOConnection, updateSSOPolicy, type SSOConnection, type SSOConnectionInput, type SSOMember,
} from "../lib/sso";
import { CodeBlock } from "../ui/CodeBlock";
import { DataTable, RowActions, TableCount } from "../ui/DataTable";
import { ConfirmDialog, Dialog, FormDialog } from "../ui/Dialog";
import { ChoiceCards, Field, SecretInput, Segmented, TextArea, TextInput } from "../ui/Field";
import { Notice } from "../ui/Notice";
import { PageHeader } from "../ui/PageHeader";
import { DangerZone, Panel } from "../ui/Panel";
import { DeniedState, QueryState } from "../ui/QueryState";
import { RelativeTime } from "../ui/RelativeTime";
import { LinesSkeleton, TableSkeleton } from "../ui/Skeleton";
import { cx, ui } from "../ui/styles";
import { Tag } from "../ui/Tag";

const MEMBER_COLUMNS = ["Member", "Email", "Company account", "Linked", { label: "Actions", srOnly: true }] as const;
const LIFETIMES = [
  { value: "86400", label: "1 day" },
  { value: "604800", label: "7 days" },
  { value: "1209600", label: "14 days" },
  { value: "2592000", label: "30 days" },
] as const;

// Owners connect the organization to its identity provider, test the
// connection, turn it on as optional and may then require it. Every change asks
// for a recent sign-in.
export function SSOSettings() {
  const me = useMe();
  const cache = useQueryClient();
  const [params] = useSearchParams();
  const isOwner = () => me.data?.is_owner === true;
  const status = createQuery(() => ({ queryKey: ["sso", "status"], queryFn: getSSOStatus, enabled: isOwner() }));
  const config = createQuery(() => ({ queryKey: ["auth-config"], queryFn: getAuthConfig }));
  const members = createQuery(() => ({ queryKey: ["sso", "members"], queryFn: listSSOMembers, enabled: isOwner() && !!status.data?.active }));
  const tested = typeof params["tested"] === "string";
  const failed = typeof params["sso_error"] === "string" ? AUTH_MESSAGES[params["sso_error"]] ?? AUTH_MESSAGES["authentication_invalid"]! : "";
  const [notice, setNotice] = createSignal(tested ? "The test sign-in succeeded. You can turn the connection on." : "");
  const [error, setError] = createSignal(failed);
  const [settingUp, setSettingUp] = createSignal(false);
  // Back from a test sign-in, an owner replacing the identity provider resumes
  // the replacement where they left it.
  const [replacing, setReplacing] = createSignal(tested);
  const [updating, setUpdating] = createSignal<SSOConnection>();
  const [discarding, setDiscarding] = createSignal<SSOConnection>();
  const [activating, setActivating] = createSignal<SSOConnection>();
  const [disabling, setDisabling] = createSignal(false);
  const [requiring, setRequiring] = createSignal<boolean>();
  const [unlinking, setUnlinking] = createSignal<SSOMember>();
  const [busy, setBusy] = createSignal(false);
  const refresh = () => Promise.all([cache.invalidateQueries({ queryKey: ["sso"] }), cache.invalidateQueries({ queryKey: ["me"] })]);
  const run = async (fn: () => Promise<void>) => {
    setBusy(true); setError("");
    try { await fn(); } catch (cause) { setError(errorMessage(cause)); } finally { setBusy(false); }
  };
  const test = (connection: SSOConnection) => run(async () => { window.location.assign((await testSSOConnection(connection.id)).redirect_url); });
  const lifetime = (seconds: number) => String(seconds) as (typeof LIFETIMES)[number]["value"];
  return <>
    <PageHeader title="Single sign-on" subtitle="Members sign in through your company identity provider. Other sign-in methods keep working unless you require single sign-on." />
    <Show when={isOwner()} fallback={<Show when={!me.isPending}><DeniedState message="Only owners can manage single sign-on." /></Show>}>
      <div class={ui.sectionStack}>
        <Show when={notice()}><Notice tone="success" onDismiss={() => setNotice("")}>{notice()}</Notice></Show>
        <Show when={error()}><Notice tone="danger" onDismiss={() => setError("")}>{error()}</Notice></Show>
        <Show when={config.data && !config.data.email_delivery}>
          <Notice tone="warning">Email sign-in is not available on this server. People who have not used Helmr before cannot link a company account on their first single sign-on; they sign in another way first and link it from Sign-in and security.</Notice>
        </Show>
        <QueryState query={status} pending={<LinesSkeleton label="Loading single sign-on" />} error={cause => loadError(cause, "Could not load single sign-on.")}>
          {data => <>
            <Panel title="Connection" actions={data().active
              ? <Tag>{data().required ? "required" : "optional"}</Tag>
              : <Show when={data().candidate} fallback={<button type="button" class={ui.compactButton} onClick={() => setSettingUp(true)}>Set up connection</button>}>
                {candidate => <Tag>{candidate().tested_at ? "tested" : "not tested"}</Tag>}
              </Show>}>
              <Show when={data().active} fallback={
                <Show when={data().candidate} fallback={<p class={cx(ui.muted, "m-0")}>Single sign-on is off. Set up a connection, test it, then turn it on.</p>}>
                  {candidate => <CandidateSetup connection={candidate()} busy={busy()} onTest={() => void test(candidate())} onActivate={() => setActivating(candidate())} onUpdate={() => setUpdating(candidate())} onDiscard={() => setDiscarding(candidate())} />}
                </Show>
              }>
                {active => <>
                  <ConnectionFacts connection={active()} />
                  <p class={cx(ui.muted, "m-0")}>
                    {data().required ? "Required: members sign in only through the identity provider; owners can also sign in as before." : "Optional: members can sign in through the identity provider or as before."}
                    {" "}{data().members_linked} of {data().members_total} members are linked.
                  </p>
                  <Show when={data().candidate}>
                    {candidate => (
                      <Notice tone="info">
                        Replacing with {candidate().protocol === "oidc" ? candidate().oidc_issuer : candidate().saml_entity_id} ({candidate().tested_at ? "tested" : "not tested"}). Members keep signing in through the current connection until you turn the new one on.
                      </Notice>
                    )}
                  </Show>
                  <div class={ui.buttonGroup}>
                    <button type="button" class={ui.secondaryButton} disabled={busy()} onClick={() => setUpdating(active())}>Update certificates or secret</button>
                    <Show when={data().candidate} fallback={<button type="button" class={ui.secondaryButton} disabled={busy()} onClick={() => setSettingUp(true)}>Replace identity provider…</button>}>
                      <button type="button" class={ui.secondaryButton} disabled={busy()} onClick={() => setReplacing(true)}>Continue replacement…</button>
                    </Show>
                  </div>
                </>}
              </Show>
            </Panel>

            <Show when={data().active}>
              <Panel title="Policy">
                <Field label="Single sign-on">
                  <Segmented label="Single sign-on" value={data().required ? "required" : "optional"} disabled={busy()}
                    options={[{ value: "optional", label: "Optional" }, { value: "required", label: "Required" }]}
                    onChange={value => setRequiring(value === "required")} />
                </Field>
                <Field label="Browser session lifetime" hint="How long a single sign-on admits a browser before members sign in through the identity provider again. CLI logins and Slack links are not affected.">
                  <Segmented label="Browser session lifetime" value={lifetime(data().session_seconds)} disabled={busy()} options={LIFETIMES}
                    onChange={value => void run(async () => { await updateSSOPolicy({ session_seconds: Number(value) }); await refresh(); setNotice("Session lifetime updated."); })} />
                </Field>
              </Panel>

              <QueryState query={members} pending={<TableSkeleton label="Loading members" columns={MEMBER_COLUMNS} />} error={cause => loadError(cause, "Could not load members.")}>
                {list => (
                  <DataTable columns={MEMBER_COLUMNS} minWidth="min-w-150" footer={<TableCount count={list().members.length} noun={{ one: "member", other: "members" }} />}>
                    <For each={list().members}>
                      {member => (
                        <tr>
                          <td>{member.display_name}</td>
                          <td class="font-mono text-chrome!">{member.email}</td>
                          <td class="font-mono text-chrome!"><Show when={member.subject} fallback={<span class={ui.muted}>Not linked</span>}>{member.subject}</Show></td>
                          <td><RelativeTime value={member.linked_at} /></td>
                          <RowActions label={`Actions for ${member.display_name}`} items={member.linked_at ? [{ label: "Unlink", tone: "danger", onSelect: () => setUnlinking(member) }] : []} />
                        </tr>
                      )}
                    </For>
                  </DataTable>
                )}
              </QueryState>

              <DangerZone title="Turn off single sign-on" action={<button type="button" class={ui.dangerOutlineButton} onClick={() => setDisabling(true)}>Turn off…</button>}>
                Retires the connection and every member's link. Browsers and CLI logins that signed in through it sign out; other sign-in methods keep working.
              </DangerZone>
            </Show>

            <Show when={settingUp()}><SetupDialog replacing={!!data().active} onClose={() => setSettingUp(false)} onCreated={async () => {
              await refresh();
              if (data().active) setReplacing(true);
              else setNotice("Connection saved. Copy the values into your identity provider, then test it.");
            }} /></Show>
            <Show when={replacing() && data().active && data().candidate}>{candidate => {
              // Read the connection before closing: the closed dialog's accessor is stale.
              const next = (open: (connection: SSOConnection) => void) => () => { const connection = candidate(); setReplacing(false); open(connection); };
              return (
                <Dialog title="Replace identity provider" onClose={() => setReplacing(false)}>
                  <CandidateSetup connection={candidate()} busy={busy()} onTest={() => void test(candidate())}
                    onActivate={next(setActivating)} onUpdate={next(setUpdating)} onDiscard={next(setDiscarding)} />
                </Dialog>
              );
            }}</Show>
            <Show when={updating()}>{connection => <UpdateDialog connection={connection()} onClose={() => setUpdating(undefined)} onUpdated={async () => { await refresh(); setNotice("Connection updated. Test it again before turning it on."); }} />}</Show>
            <Show when={discarding()}>{connection => (
              <ConfirmDialog title="Discard connection" confirmLabel="Discard" busyLabel="Discarding…" tone="danger" onClose={() => setDiscarding(undefined)} onConfirm={async () => { await deleteSSOConnection(connection().id); await refresh(); }}>
                The connection is deleted. Nobody has signed in through it.
              </ConfirmDialog>
            )}</Show>
            <Show when={activating()}>{connection => (
              <ConfirmDialog title={data().active ? "Switch identity provider" : "Turn on single sign-on"} confirmLabel={data().active ? "Switch" : "Turn on"} busyLabel={data().active ? "Switching…" : "Turning on…"} onClose={() => setActivating(undefined)} onConfirm={async () => { const switching = !!data().active; await activateSSOConnection(connection().id); await refresh(); setNotice(switching ? "Identity provider switched. Members link their company account again on their next single sign-on." : "Single sign-on is on. Members can sign in through the identity provider or link it from Sign-in and security."); }}>
                <Show when={data().active} fallback={<>Members can sign in through the identity provider. Other sign-in methods keep working.</>}>
                  The current connection retires with every member's link. Members link again through the new connection, and browsers and CLI logins that signed in through the old one sign out.
                </Show>
              </ConfirmDialog>
            )}</Show>
            <Show when={requiring() !== undefined}>
              <ConfirmDialog title={requiring() ? "Require single sign-on" : "Make single sign-on optional"} confirmLabel={requiring() ? "Require" : "Make optional"} busyLabel="Saving…" tone={requiring() ? "danger" : "default"} onClose={() => setRequiring(undefined)}
                onConfirm={async () => { const result = await updateSSOPolicy({ required: requiring()! }); await refresh(); setNotice(requiring() ? `Single sign-on is required. ${result.revoked_cli_sessions} CLI logins and ${result.revoked_slack_links} Slack links were signed out.` : "Single sign-on is optional."); }}>
                <Show when={requiring()} fallback={<>Members can sign in as before again. Nothing is signed out.</>}>
                  <div class={ui.blockStack}>
                    <p class="m-0">Members reach the organization only through the identity provider. Owners can still sign in as before, so an identity provider outage cannot lock the organization out. CLI logins not approved through single sign-on and every Slack link sign out once, and members link Slack again.</p>
                    <p class="m-0">Disabling someone at the identity provider ends their browser access within the session lifetime. Their CLI logins and Slack links keep working until you remove or suspend them here.</p>
                    <p class="m-0">You must be signed in through the connection yourself.</p>
                  </div>
                </Show>
              </ConfirmDialog>
            </Show>
            <Show when={disabling()}>
              <ConfirmDialog title="Turn off single sign-on" confirmLabel="Turn off" busyLabel="Turning off…" tone="danger" onClose={() => setDisabling(false)} onConfirm={async () => { await deleteSSOConnection(data().active!.id); await refresh(); setNotice("Single sign-on is off."); }}>
                The connection retires with every member's link, and required single sign-on ends. Browsers and CLI logins that signed in through it sign out.
              </ConfirmDialog>
            </Show>
            <Show when={unlinking()}>{member => (
              <ConfirmDialog title="Unlink company account" confirmLabel="Unlink" busyLabel="Unlinking…" tone="danger" target={member().display_name} onClose={() => setUnlinking(undefined)} onConfirm={async () => { await unlinkSSOMember(member().member_id); await refresh(); }}>
                Their browsers and CLI logins that signed in through single sign-on sign out. They can link a company account again.
              </ConfirmDialog>
            )}</Show>
          </>}
        </QueryState>
      </div>
    </Show>
  </>;
}

// The steps from a saved connection to turning it on: copy Helmr's values into
// the identity provider, test with the owner's own company account, turn on.
function CandidateSetup(props: { connection: SSOConnection; busy: boolean; onTest: () => void; onActivate: () => void; onUpdate: () => void; onDiscard: () => void }) {
  return (
    <div class={ui.blockStack}>
      <p class={cx(ui.muted, "m-0")}>Copy these values into your identity provider, then test the connection by signing in with your own company account.</p>
      <ServiceProviderValues connection={props.connection} />
      <ConnectionFacts connection={props.connection} />
      <div class={ui.buttonGroup}>
        <button type="button" class={ui.secondaryButton} disabled={props.busy} onClick={props.onTest}>Test sign-in</button>
        <button type="button" class={ui.button} disabled={props.busy || !props.connection.tested_at} onClick={props.onActivate}>Turn on</button>
        <button type="button" class={ui.secondaryButton} disabled={props.busy} onClick={props.onUpdate}>Update</button>
        <button type="button" class={ui.dangerOutlineButton} disabled={props.busy} onClick={props.onDiscard}>Discard</button>
      </div>
    </div>
  );
}

function ConnectionFacts(props: { connection: SSOConnection }) {
  return (
    <dl class="m-0 grid gap-2.5 sm:grid-cols-2">
      <Fact label="Protocol">{props.connection.protocol.toUpperCase()}</Fact>
      <Show when={props.connection.protocol === "oidc"} fallback={<>
        <Fact label="Identity provider entity ID">{props.connection.saml_entity_id}</Fact>
        <Fact label="Sign-in URL">{props.connection.saml_sso_url}</Fact>
      </>}>
        <Fact label="Issuer">{props.connection.oidc_issuer}</Fact>
        <Fact label="Client ID">{props.connection.oidc_client_id}</Fact>
      </Show>
    </dl>
  );
}

function Fact(props: { label: string; children: string | undefined }) {
  return (
    <div class="grid gap-0.75">
      <dt class={cx("m-0", ui.microLabel)}>{props.label}</dt>
      <dd class="m-0 font-mono text-chrome text-console-text [overflow-wrap:anywhere]">{props.children ?? "—"}</dd>
    </div>
  );
}

function ServiceProviderValues(props: { connection: SSOConnection }) {
  return (
    <Show when={props.connection.protocol === "saml"} fallback={<Field label="Redirect URI"><CodeBlock code={props.connection.oidc_redirect_url ?? ""} copy label="Redirect URI" /></Field>}>
      <Field label="ACS URL"><CodeBlock code={props.connection.service_provider_acs_url ?? ""} copy label="ACS URL" /></Field>
      <Field label="Audience (SP entity ID)"><CodeBlock code={props.connection.service_provider_entity_id ?? ""} copy label="Audience" /></Field>
      <p class={cx(ui.muted, "m-0")}>Send a persistent or email NameID, and the email address as the NameID or an email attribute.</p>
    </Show>
  );
}

function certificates(value: string): string[] {
  return value.split(/(?=-----BEGIN CERTIFICATE-----)/).map(c => c.trim()).filter(Boolean);
}

function SetupDialog(props: { replacing: boolean; onClose: () => void; onCreated: () => Promise<void> }) {
  const [protocol, setProtocol] = createSignal<"oidc" | "saml">("oidc");
  const [issuer, setIssuer] = createSignal(""); const [clientID, setClientID] = createSignal(""); const [secret, setSecret] = createSignal("");
  const [metadata, setMetadata] = createSignal(""); const [entityID, setEntityID] = createSignal(""); const [ssoURL, setSSOURL] = createSignal(""); const [certs, setCerts] = createSignal("");
  const [busy, setBusy] = createSignal(false); const [error, setError] = createSignal("");
  const input = (): SSOConnectionInput => protocol() === "oidc"
    ? { protocol: "oidc", issuer: issuer().trim(), client_id: clientID().trim(), client_secret: secret() }
    : metadata().trim() ? { protocol: "saml", metadata_xml: metadata() } : { protocol: "saml", entity_id: entityID().trim(), sso_url: ssoURL().trim(), certificates: certificates(certs()) };
  const ready = () => protocol() === "oidc" ? !!issuer().trim() && !!clientID().trim() && !!secret() : !!metadata().trim() || (!!entityID().trim() && !!ssoURL().trim() && !!certs().trim());
  const submit = async () => {
    setBusy(true); setError("");
    try { await createSSOConnection(input()); await props.onCreated(); props.onClose(); }
    catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };
  return (
    <FormDialog title={props.replacing ? "Replace identity provider" : "Set up connection"}
      intro={props.replacing ? "Members keep signing in through the current connection until you test the new one and turn it on." : "Nothing changes for members until you test the connection and turn it on."}
      submitLabel={props.replacing ? "Continue" : "Save connection"} busyLabel="Saving…" busy={busy()} disabled={!ready()} error={error()} onSubmit={() => void submit()} onClose={props.onClose}>
      <ChoiceCards label="Protocol" name="sso-protocol" value={protocol()} onChange={setProtocol} options={[
        { value: "oidc", label: "OIDC", description: "Okta, Microsoft Entra ID, Google Workspace and most identity providers." },
        { value: "saml", label: "SAML", description: "When your identity provider offers only SAML." },
      ]} />
      <Show when={protocol() === "oidc"} fallback={<>
        <Field label="Identity provider metadata XML" hint="Paste the metadata, or leave it empty and fill in the fields below."><TextArea value={metadata()} onInput={e => setMetadata(e.currentTarget.value)} rows={5} /></Field>
        <Show when={!metadata().trim()}>
          <Field label="Identity provider entity ID"><TextInput value={entityID()} onInput={e => setEntityID(e.currentTarget.value)} /></Field>
          <Field label="Sign-in URL" hint="The HTTP-Redirect single sign-on URL."><TextInput type="url" value={ssoURL()} onInput={e => setSSOURL(e.currentTarget.value)} /></Field>
          <Field label="Signing certificates" hint="One or more PEM certificates."><TextArea value={certs()} onInput={e => setCerts(e.currentTarget.value)} rows={5} /></Field>
        </Show>
      </>}>
        <Field label="Issuer" hint="The HTTPS issuer URL, without a trailing slash unless your provider uses one."><TextInput type="url" value={issuer()} onInput={e => setIssuer(e.currentTarget.value)} /></Field>
        <Field label="Client ID"><TextInput value={clientID()} onInput={e => setClientID(e.currentTarget.value)} /></Field>
        <Field label="Client secret"><SecretInput value={secret()} onInput={e => setSecret(e.currentTarget.value)} /></Field>
      </Show>
    </FormDialog>
  );
}

function UpdateDialog(props: { connection: SSOConnection; onClose: () => void; onUpdated: () => Promise<void> }) {
  const [secret, setSecret] = createSignal(""); const [metadata, setMetadata] = createSignal(""); const [ssoURL, setSSOURL] = createSignal(props.connection.saml_sso_url ?? ""); const [certs, setCerts] = createSignal("");
  const [busy, setBusy] = createSignal(false); const [error, setError] = createSignal("");
  const oidc = () => props.connection.protocol === "oidc";
  const ready = () => oidc() ? !!secret() : !!metadata().trim() || (!!ssoURL().trim() && !!certs().trim());
  const submit = async () => {
    setBusy(true); setError("");
    try {
      await updateSSOConnection(props.connection.id, oidc() ? { client_secret: secret() } : metadata().trim() ? { metadata_xml: metadata() } : { sso_url: ssoURL().trim(), certificates: certificates(certs()) });
      await props.onUpdated(); props.onClose();
    } catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };
  return (
    <FormDialog title="Update connection" intro="The identity provider stays the same; to change it, replace the identity provider." submitLabel="Update" busyLabel="Updating…" busy={busy()} disabled={!ready()} error={error()} onSubmit={() => void submit()} onClose={props.onClose}>
      <Show when={oidc()} fallback={<>
        <Field label="Identity provider metadata XML" hint="Paste new metadata, or leave it empty and fill in the fields below."><TextArea value={metadata()} onInput={e => setMetadata(e.currentTarget.value)} rows={5} /></Field>
        <Show when={!metadata().trim()}>
          <Field label="Sign-in URL"><TextInput type="url" value={ssoURL()} onInput={e => setSSOURL(e.currentTarget.value)} /></Field>
          <Field label="Signing certificates" hint="Include the current and the next certificate while the identity provider rotates."><TextArea value={certs()} onInput={e => setCerts(e.currentTarget.value)} rows={5} /></Field>
        </Show>
      </>}>
        <Field label="New client secret"><SecretInput value={secret()} onInput={e => setSecret(e.currentTarget.value)} /></Field>
      </Show>
    </FormDialog>
  );
}
