import { AuthConfirmation } from "./routes/auth-confirmation";
import { AuthenticationNotice } from "./components/AuthenticationNotice";
import { OperatorKeys } from "./routes/operator-keys";
import { AccountRepair } from "./routes/account-repair";
import { Organizations } from "./routes/organizations";
import { OperatorLogin } from "./routes/operator-login";
import { Setup } from "./routes/setup";
import { A, Navigate, Route, Router, useLocation } from "@solidjs/router";
import { environmentChip } from "./features/projects/display";
import type { JSX } from "solid-js";
import { Show } from "solid-js";
import { AppBar, type NavItem } from "./components/AppBar";
import { ProfileMenu } from "./components/ProfileMenu";
import { RequireAuth } from "./components/RequireAuth";
import { RequireAdmin } from "./components/RequireAdmin";
import { AdminLayout } from "./components/AdminLayout";
import { ScopeSelector } from "./components/ScopeSelector";
import { CommandPalette } from "./components/CommandPalette";
import { attentionTotal, useAttentionCounts } from "./lib/attention";
import { SettingsLayout } from "./components/SettingsLayout";
import { hasPermission, onboardingRedirectPath, type AuthRequirement } from "./lib/auth";
import { useMe } from "./lib/me";
import { ScopeProvider, useScope } from "./lib/scope";
import { logout } from "./lib/auth";
import { formatRole } from "./components/ProfileMenu";
import { Notice } from "./ui/Notice";
import { Page } from "./ui/Page";
import { cx, ui } from "./ui/styles";
import { Overview } from "./routes/overview";
import { Invite } from "./routes/invite";
import { Login } from "./routes/login";
import { AuthSlackLink } from "./routes/auth-slack-link";
import { SlackConnect } from "./routes/slack-connect";
import { AuthSlackCallback } from "./routes/auth-slack-callback";
import { AccountSettings } from "./routes/account-settings";
import { AccountSecurity } from "./routes/account-security";
import { Ownership } from "./routes/ownership";
import { AuthGitHubCallback } from "./routes/auth-callback";
import { SessionList } from "./routes/sessions";
import { TriggerList } from "./routes/triggers";
import { AgentsSection, AgentList, AgentDetail, AgentSessionsTab, AgentTriggersTab, AgentSlackTab, AgentAccessTab } from "./routes/agents";
import { ComputersSection, ComputerList, TemplateList } from "./routes/computers";
import { Deployments } from "./routes/deployments";
import { DeploymentDetail } from "./routes/deployment-detail";
import { ApiKeys } from "./routes/api-keys";
import { Secrets } from "./routes/secrets";
import { Groups } from "./routes/groups";
import { GroupDetail } from "./routes/group-detail";
import { AuditEvents } from "./routes/audit-events";
import { Roles } from "./routes/roles";
import { RoleDetail } from "./routes/role-detail";
import { Members } from "./routes/members";
import { MemberDetail } from "./routes/member-detail";
import { ProjectSettings } from "./routes/project-settings";
import { Environments } from "./routes/environments";
import { ProjectNew } from "./routes/project-new";
import { OrganizationNew } from "./routes/organization-new";
import { AccessRequired } from "./routes/access-required";
import { Device } from "./routes/device";
import { ComputerDetail } from "./routes/computer-detail";
import { QuestionDetail } from "./routes/question-detail";
import { SessionDetail } from "./routes/session-detail";
import { NotFound, NotFoundWindow } from "./routes/not-found";
import { AdminRegions } from "./routes/admin-regions";
import { AdminWorkerGroups } from "./routes/admin-worker-groups";

function SettingsLink() {
  const location = useLocation();
  const active = () => location.pathname.startsWith("/settings");
  return (
    <A
      href="/settings/project"
      class={ui.iconButton}
      data-open={active() ? "true" : "false"}
      aria-current={active() ? "page" : undefined}
      aria-label="Settings"
      title="Settings"
    >
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <circle cx="12" cy="12" r="3" />
        <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
      </svg>
    </A>
  );
}

// The Settings pages about the signed-in account rather than a Project or the
// organization.
const ACCOUNT_SETTINGS_PATHS = ["/settings/account", "/settings/security"];

// The phone menu's Settings and account entries.
function DrawerAccount(props: { close: () => void }) {
  const me = useMe();
  const location = useLocation();
  const item = (active: boolean) => cx(ui.drawerItem, active ? ui.activeBar : ui.drawerItemIdle);
  const inAccount = () => ACCOUNT_SETTINGS_PATHS.includes(location.pathname);
  return (
    <div class="flex flex-col gap-0.5 p-2">
      <A href="/settings/project" class={item(location.pathname.startsWith("/settings") && !inAccount())} onClick={props.close}>Settings</A>
      <A href="/settings/account" class={item(inAccount())} onClick={props.close}>Profile</A>
      <A href="/organizations" class={item(false)} onClick={props.close}>Switch organization</A>

      <button
        type="button"
        class={cx(ui.drawerItem, ui.drawerItemIdle)}
        onClick={() => {
          props.close();
          void logout();
        }}
      >
        Sign out
      </button>
      <p class="m-0 truncate px-2.5 py-2 font-mono text-chrome text-console-muted">
        {me.data?.display_name?.trim() || me.data?.user_id || "Account"} · {formatRole(me.data?.is_owner ? "owner" : "member")}<Show when={me.data?.org_name}>{name => <> · {name()}</>}</Show>
      </p>
    </div>
  );
}

function AppShell(props: { children?: JSX.Element }) {
  const scope = useScope();
  const attention = useAttentionCounts();
  const me = useMe();
  const canRead = (permission: string) => hasPermission(me.data, permission, { projectID: scope.selectedProjectID(), environmentID: scope.selectedEnvironmentID() });
  const nav = (): NavItem[] => [
    { href: "/", label: "Overview", count: () => (attention.data ? attentionTotal(attention.data.counts) : undefined) },
    { href: "/agents", label: "Agents", match: ["/sessions", "/triggers"] },
    ...(canRead("computers.read") ? [{ href: "/computers", label: "Computers", match: ["/templates"] }] : []),
    ...(canRead("deployments.read") ? [{ href: "/deployments", label: "Deployments" }] : []),
  ];
  const environmentColor = () => environmentChip(scope.selectedEnvironment())?.colorHex;
  return (
    <div class={"min-h-dvh bg-transparent font-sans text-sm text-console-text"}>
      <AppBar
        leading={<ScopeSelector />}
        nav={nav()}
        trailing={<><CommandPalette /><div class="flex items-center gap-2 max-[720px]:hidden"><SettingsLink /><ProfileMenu /></div></>}
        environmentColor={environmentColor()}
        drawerEnd={(close) => <DrawerAccount close={close} />}
      />
      <Show when={scope.error()}>
        <Notice tone="danger" variant="bar" class="font-medium">Could not load projects.</Notice>
      </Show>
      <div role="status" aria-live="polite">
        <Show when={scope.notice()}>
          {(notice) => (
            <Notice tone="info" variant="bar" role="none" onDismiss={() => scope.dismissNotice()}>
              {notice().kind === "switched"
                ? `Switched to ${notice().projectName} / ${notice().environmentName} from the link.`
                : `The link's project or environment is not available; showing ${notice().projectName} / ${notice().environmentName}.`}
            </Notice>
          )}
        </Show>
      </div>
      <main class={"min-w-0 flex-1"}>{props.children}</main>
    </div>
  );
}

const wrap = (Inner: () => JSX.Element) => () => (
  <RequireAuth>
    <ScopeProvider>
      <AppShell>
        <Inner />
      </AppShell>
    </ScopeProvider>
  </RequireAuth>
);

// A page with sub-pages: the layout stays mounted while its child routes change.
const wrapLayout = (Layout: (props: { children?: JSX.Element }) => JSX.Element) => (props: { children?: JSX.Element }) => (
  <RequireAuth>
    <ScopeProvider>
      <AppShell>
        <Layout>{props.children}</Layout>
      </AppShell>
    </ScopeProvider>
  </RequireAuth>
);

const wrapSettings = (Inner: () => JSX.Element, requirement: AuthRequirement = "project") => () => (
  <RequireAuth requirement={requirement}>
    <ScopeProvider>
      <AppShell>
        <SettingsLayout>
          <Inner />
        </SettingsLayout>
      </AppShell>
    </ScopeProvider>
  </RequireAuth>
);

// Signed-in accounts see a missing page inside the console; anyone else sees it
// in a window, without being sent to sign in first.
function Missing() {
  const me = useMe();
  const inConsole = () => !!me.data && !onboardingRedirectPath(me.data, "organization");
  return (
    <Show when={!me.isPending}>
      <Show when={inConsole()} fallback={<NotFoundWindow />}>
        <ScopeProvider>
          <AppShell>
            <Page>
              <NotFound />
            </Page>
          </AppShell>
        </ScopeProvider>
      </Show>
    </Show>
  );
}

const wrapAdmin = (Inner: () => JSX.Element) => () => (
  <RequireAdmin>
    <AdminLayout>
      <Inner />
    </AdminLayout>
  </RequireAdmin>
);

export function App() {
  return (
    <Router root={props => <><AuthenticationNotice />{props.children}</>}>
      <Route path="/" component={wrap(Overview)} />
      <Route path="/login" component={Login} />
      <Route path="/organizations" component={Organizations} />
      <Route path="/admin/login" component={OperatorLogin} />
      <Route path="/setup" component={Setup} />
      <Route path="/invite" component={Invite} />
      <Route path="/auth/device" component={() => <RequireAuth requirement="organization"><Device /></RequireAuth>} />
      <Route path="/auth/slack/callback" component={AuthSlackCallback} />
      <Route path="/auth/slack/link" component={AuthSlackLink} />
      <Route path="/auth/slack/connect" component={SlackConnect} />
      <Route path="/auth/confirmation" component={AuthConfirmation} />
      <Route path="/auth/github/callback" component={AuthGitHubCallback} />
      <Route path="/access-required" component={() => <RequireAuth requirement="session"><AccessRequired /></RequireAuth>} />
      <Route path="/organizations/new" component={() => <RequireAuth requirement="session"><OrganizationNew /></RequireAuth>} />

      <Route component={wrapLayout(AgentsSection)}>
        <Route path="/agents" component={AgentList} />
        <Route path="/sessions" component={SessionList} />
        <Route path="/triggers" component={TriggerList} />
      </Route>
      <Route path="/agents/:agent" component={wrapLayout(AgentDetail)}>
        <Route path="/" component={AgentSessionsTab} />
        <Route path="/triggers" component={AgentTriggersTab} />
        <Route path="/slack" component={AgentSlackTab} />
        <Route path="/access" component={AgentAccessTab} />
      </Route>
      <Route path="/sessions/:session_id" component={wrap(SessionDetail)} />
      <Route path="/sessions/:session_id/turns/:turn_id/asks/:ask_id" component={wrap(QuestionDetail)} />
      <Route component={wrapLayout(ComputersSection)}>
        <Route path="/computers" component={ComputerList} />
        <Route path="/templates" component={TemplateList} />
      </Route>
      <Route path="/computers/:computer_id" component={wrap(ComputerDetail)} />
      <Route path="/deployments" component={wrap(Deployments)} />
      <Route path="/deployments/:deployment_id" component={wrap(DeploymentDetail)} />
      <Route path="/projects/new" component={() => <RequireAuth requirement="organization"><ProjectNew /></RequireAuth>} />

      <Route path="/admin" component={() => <Navigate href="/admin/worker-groups" />} />
      <Route path="/admin/audit-events" component={wrapAdmin(() => <Page><AuditEvents adminView /></Page>)} />
      <Route path="/admin/operator-keys" component={wrapAdmin(OperatorKeys)} />
      <Route path="/admin/account-repair" component={wrapAdmin(AccountRepair)} />
      <Route path="/admin/regions" component={wrapAdmin(AdminRegions)} />
      <Route path="/admin/worker-groups" component={wrapAdmin(AdminWorkerGroups)} />

      <Route path="/settings" component={() => <Navigate href="/settings/project" />} />
      <Route path="/settings/account" component={wrapSettings(AccountSettings, "session")} />
      <Route path="/settings/security" component={wrapSettings(AccountSecurity, "session")} />
      <Route path="/settings/ownership" component={wrapSettings(Ownership, "organization")} />
      <Route path="/settings/project" component={wrapSettings(ProjectSettings)} />
      <Route path="/settings/environments" component={wrapSettings(Environments)} />
      <Route path="/settings/secrets" component={wrapSettings(Secrets)} />
      <Route path="/settings/api-keys" component={wrapSettings(ApiKeys)} />
      <Route path="/settings/groups" component={wrapSettings(Groups)} />
      <Route path="/settings/groups/:group" component={wrapSettings(GroupDetail)} />
      <Route path="/settings/audit-events" component={wrapSettings(() => <AuditEvents />, "organization")} />
      <Route path="/settings/roles" component={wrapSettings(Roles)} />
      <Route path="/settings/roles/:role" component={wrapSettings(RoleDetail)} />
      <Route path="/settings/members" component={wrapSettings(Members)} />
      <Route path="/settings/members/:member" component={wrapSettings(MemberDetail, "organization")} />
      <Route path="*" component={Missing} />
    </Router>
  );
}
