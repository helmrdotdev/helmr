import { useMe } from "../lib/me";
import { IdentitySettings } from "../features/slack/IdentitySettings";
import { statusLabel } from "../ui/label";
import { PageHeader } from "../ui/PageHeader";
import { KeyValue, Panel } from "../ui/Panel";
import { QueryState } from "../ui/QueryState";
import { ui } from "../ui/styles";

export function AccountSettings() {
  const me = useMe();
  return <>
    <PageHeader title="My account" />
    <QueryState query={me} pending="Loading your account..." error={() => "Your account could not be loaded."}>
      {(account) => (
        <div class={ui.sectionStack}>
          <Panel title="Profile">
            <dl class="m-0 grid grid-cols-3 gap-4 max-sm:grid-cols-1">
              <KeyValue label="Name">{account().display_name?.trim() || undefined}</KeyValue>
              <KeyValue label="Role">{account().role ? statusLabel(account().role!) : undefined}</KeyValue>
              <KeyValue label="Organization">{account().org_name || undefined}</KeyValue>
            </dl>
          </Panel>
          <IdentitySettings userID={account().user_id} organizationID={account().org_id ?? ""} />
        </div>
      )}
    </QueryState>
  </>;
}
