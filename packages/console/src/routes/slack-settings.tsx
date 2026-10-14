import { useMe } from "../lib/me";
import { IdentitySettings } from "../features/slack/IdentitySettings";
import { PageHeader } from "../ui/PageHeader";
import { QueryState } from "../ui/QueryState";
import { ui } from "../ui/styles";

export function SlackSettings() {
  const me = useMe();
  return <>
    <PageHeader
      title="Your Slack account"
      subtitle="Link your Slack account to your Helmr account. This personal link works across all Agent apps in that Slack workspace, using your current Helmr permissions."
    />
    <div class={ui.windowStack}>
      <p class={ui.muted}>To connect an Agent, open its Slack connection from the deployment’s Agents list.</p>
      <QueryState query={me} pending="Loading your account..." error={() => "Your account could not be loaded."}>
        {(account) => <IdentitySettings userID={account().user_id} organizationID={account().org_id ?? ""} />}
      </QueryState>
    </div>
  </>;
}
