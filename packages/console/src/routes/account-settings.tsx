import { A } from "@solidjs/router";
import { memberHref } from "../lib/permissions";
import { OwnershipSettings } from "../features/access/OwnershipSettings";
import { Show } from "solid-js";
import { useMe } from "../lib/me";
import { IdentitySettings } from "../features/slack/IdentitySettings";
import { PageHeader } from "../ui/PageHeader";
import { KeyValue, Panel } from "../ui/Panel";
import { QueryState } from "../ui/QueryState";
import { PageSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";

export function AccountSettings() {
  const me = useMe();
  return <>
    <PageHeader title="My account" />
    <QueryState query={me} pending={<PageSkeleton label="Loading your account" sections={["Profile", "Your Slack identity"]} />} error={() => "Your account could not be loaded."}>
      {(account) => (
        <div class={ui.sectionStack}>
          <Panel title="Profile">
            <dl class="m-0 grid grid-cols-3 gap-4 max-sm:grid-cols-1">
              <KeyValue label="Name">{account().display_name?.trim() || undefined}</KeyValue>
              <KeyValue label="Ownership">{account().is_owner ? "Owner" : "Not an owner"}</KeyValue>
              <KeyValue label="Organization">{account().org_name || undefined}</KeyValue>
            </dl>
          </Panel>
          <Show when={account().membership_id}>{membership => <Panel title="My access" actions={<A href={memberHref(membership())} class={ui.compactButton}>View my access</A>}><p class={ui.muted}>The permissions you have and where each one comes from: direct assignments, groups and all members.</p></Panel>}</Show>
          <Show when={account().is_owner}><OwnershipSettings /></Show>
          <IdentitySettings userID={account().user_id} organizationID={account().org_id ?? ""} />
        </div>
      )}
    </QueryState>
  </>;
}
