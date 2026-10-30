import { A } from "@solidjs/router";
import { useQueryClient } from "@tanstack/solid-query";
import { createSignal, Show } from "solid-js";
import { ChangeEmailDialog } from "../features/account/ChangeEmailDialog";
import { IdentitySettings } from "../features/slack/IdentitySettings";
import { updateDisplayName } from "../lib/account";
import { errorMessage } from "../lib/error";
import { ME_QUERY_KEY, useMe } from "../lib/me";
import { memberHref } from "../lib/permissions";
import { Field, TextInput } from "../ui/Field";
import { InlineStatus, Notice } from "../ui/Notice";
import { PageHeader } from "../ui/PageHeader";
import { KeyValue, Panel } from "../ui/Panel";
import { QueryState } from "../ui/QueryState";
import { PageSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";

// Who the signed-in account is and how it belongs to the tab's organization.
// Sign-in methods and account-wide revocation are on Sign-in and security.
export function AccountSettings() {
  const me = useMe();
  return <>
    <PageHeader title="Profile" />
    <QueryState query={me} pending={<PageSkeleton label="Loading your profile" sections={["Your profile", "Slack identity"]} />} error={() => "Your account could not be loaded."}>
      {(account) => (
        <div class={ui.sectionStack}>
          <ProfilePanel displayName={account().display_name} email={account().primary_email} emailChange={account().email_delivery} />
          <Show when={account().membership_id}>{membership => (
            <Panel title="Membership" actions={<A href={memberHref(membership())} class={ui.compactButton}>View my access</A>}>
              <dl class="m-0 grid grid-cols-3 gap-4 max-sm:grid-cols-1">
                <KeyValue label="Organization">{account().org_name || undefined}</KeyValue>
                <KeyValue label="Ownership">{account().is_owner ? "Owner" : "Member"}</KeyValue>
              </dl>
            </Panel>
          )}</Show>
          <Show when={account().org_id}>{org => <IdentitySettings userID={account().user_id} organizationID={org()} />}</Show>
        </div>
      )}
    </QueryState>
  </>;
}

function ProfilePanel(props: { displayName: string; email: string; emailChange: boolean }) {
  const cache = useQueryClient();
  const [name, setName] = createSignal<string>();
  const [busy, setBusy] = createSignal(false);
  const [saved, setSaved] = createSignal(false);
  const [error, setError] = createSignal("");
  const [changingEmail, setChangingEmail] = createSignal(false);
  const [emailChanged, setEmailChanged] = createSignal(false);
  const current = () => name() ?? props.displayName;
  const save = async () => {
    setBusy(true); setError(""); setSaved(false);
    try {
      await updateDisplayName(current().trim());
      await cache.invalidateQueries({ queryKey: ME_QUERY_KEY });
      setName(undefined); setSaved(true);
    } catch (cause) { setError(errorMessage(cause)); }
    finally { setBusy(false); }
  };
  return (
    <Panel title="Your profile">
      <form class="flex flex-wrap items-end gap-2" onSubmit={e => { e.preventDefault(); void save(); }}>
        <Field label="Display name" flush error={error()} class="w-80 max-w-full">
          <TextInput value={current()} onInput={e => { setName(e.currentTarget.value); setSaved(false); }} required maxlength={200} />
        </Field>
        <button type="submit" class={ui.secondaryButton} disabled={busy() || current().trim() === props.displayName}>{busy() ? "Saving…" : "Save"}</button>
        <Show when={saved()}><InlineStatus>Saved</InlineStatus></Show>
      </form>
      <dl class="m-0 grid grid-cols-3 gap-4 max-sm:grid-cols-1">
        <KeyValue label="Login email">
          <span class="inline-flex flex-wrap items-center gap-2">
            <span class="font-mono text-chrome">{props.email}</span>
            <Show when={props.emailChange}><button type="button" class={ui.compactButton} onClick={() => setChangingEmail(true)}>Change…</button></Show>
          </span>
        </KeyValue>
      </dl>
      <Show when={emailChanged()}>
        <Notice tone="success" onDismiss={() => setEmailChanged(false)}>Login email changed. Other browsers signed in with the previous email must sign in again. CLI and Slack access stay.</Notice>
      </Show>
      <Show when={changingEmail()}><ChangeEmailDialog onClose={() => setChangingEmail(false)} onChanged={() => setEmailChanged(true)} /></Show>
    </Panel>
  );
}
