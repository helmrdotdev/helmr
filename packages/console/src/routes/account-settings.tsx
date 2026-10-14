import { useMe } from "../lib/me";
import { IdentitySettings } from "../features/slack/IdentitySettings";
import { statusLabel } from "../ui/label";
import { PageHeader } from "../ui/PageHeader";
import { KeyValue, Panel } from "../ui/Panel";
import { QueryState } from "../ui/QueryState";
import { cx, ui } from "../ui/styles";

export function AccountSettings() {
  const me = useMe();
  return <>
    <PageHeader
      title="My account"
      subtitle="Your profile in this organization and the Slack identity linked to it."
    />
    <QueryState query={me} pending="Loading your account..." error={() => "Your account could not be loaded."}>
      {(account) => (
        <div class={cx(ui.sectionStack, "max-w-220")}>
          <Panel title="Profile">
            <dl class="m-0 grid grid-cols-3 gap-4 max-sm:grid-cols-1">
              <KeyValue label="Name">{account().display_name?.trim() || undefined}</KeyValue>
              <KeyValue label="Role">{account().role ? statusLabel(account().role!) : undefined}</KeyValue>
              <KeyValue label="Organization">{account().org_name || undefined}</KeyValue>
            </dl>
          </Panel>
          <div class={ui.windowStack}>
            <IdentitySettings userID={account().user_id} organizationID={account().org_id ?? ""} />
            <p class={ui.muted}>Your Slack identity works across all Agent apps in a Slack workspace, using your current Helmr permissions. To connect an Agent, open Agents, choose the Agent and open its Slack tab.</p>
          </div>
        </div>
      )}
    </QueryState>
  </>;
}
