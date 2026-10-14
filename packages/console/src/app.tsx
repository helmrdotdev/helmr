import { A, Navigate, Route, Router, useLocation } from "@solidjs/router";
import type { JSX } from "solid-js";
import { Show } from "solid-js";
import { AppBar, type NavItem } from "./components/AppBar";
import { ProfileMenu } from "./components/ProfileMenu";
import { RequireAuth } from "./components/RequireAuth";
import { RequireAdmin } from "./components/RequireAdmin";
import { AdminLayout } from "./components/AdminLayout";
import { ScopeSwitcher } from "./components/ScopeSwitcher";
import { SettingsLayout } from "./components/SettingsLayout";
import { isProtectedEnvironment } from "./lib/projects";
import { ScopeProvider, useScope } from "./lib/scope";
import { BrandMark } from "./ui/BrandMark";
import { Notice } from "./ui/Notice";
import { ui } from "./ui/styles";
import { Overview } from "./routes/overview";
import { Invite } from "./routes/invite";
import { Login } from "./routes/login";
import { AuthSlackLink } from "./routes/auth-slack-link";
import { SlackConnect } from "./routes/slack-connect";
import { AuthSlackCallback } from "./routes/auth-slack-callback";
import { SlackSettings } from "./routes/slack-settings";
import { AuthGitHubCallback } from "./routes/auth-github-callback";
import { AuthMagicLinkCallback } from "./routes/auth-magic-link-callback";
import { Templates, AgentDefinitionDetail, TemplateDetail } from "./routes/definitions";
import { Agents, AgentDetail } from "./routes/agents";
import { Sessions } from "./routes/sessions";
import { Computers } from "./routes/computers";
import { Deployments } from "./routes/deployments";
import { DeploymentDetail } from "./routes/deployment-detail";
import { ApiKeys } from "./routes/api-keys";
import { Secrets } from "./routes/secrets";
import { Members } from "./routes/members";
import { Projects } from "./routes/projects";
import { Environments } from "./routes/environments";
import { ProjectNew } from "./routes/project-new";
import { OrganizationNew } from "./routes/organization-new";
import { AccessRequired } from "./routes/access-required";
import { Device } from "./routes/device";
import { ComputerDetail } from "./routes/computer-detail";
import { QuestionDetail } from "./routes/question-detail";
import { SessionDetail } from "./routes/session-detail";
import { AdminRegions } from "./routes/admin-regions";
import { AdminWorkerGroups } from "./routes/admin-worker-groups";

const CONSOLE_NAV: NavItem[] = [
  { href: "/", label: "Overview" },
  { href: "/agents", label: "Agents" },
  { href: "/sessions", label: "Sessions" },
  { href: "/templates", label: "Templates" },
  { href: "/computers", label: "Computers" },
  { href: "/deployments", label: "Deployments" },
];

function SettingsLink() {
  const location = useLocation();
  const active = () => location.pathname.startsWith("/settings");
  return (
    <A
      href="/settings/projects"
      class={ui.iconButton}
      data-open={active() ? "true" : "false"}
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

function AppShell(props: { children?: JSX.Element }) {
  const scope = useScope();
  const protectedColor = () => {
    const environment = scope.selectedEnvironment();
    return environment && isProtectedEnvironment(environment) ? environment.color_hex : undefined;
  };
  return (
    <div class={"min-h-dvh bg-transparent font-sans text-sm text-console-text"}>
      <AppBar
        leading={<ScopeSwitcher />}
        nav={CONSOLE_NAV}
        trailing={<><SettingsLink /><ProfileMenu /></>}
        protectedColor={protectedColor()}
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
                : `The link's Project or Environment is not available; showing ${notice().projectName} / ${notice().environmentName}.`}
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

const wrapSettings = (Inner: () => JSX.Element) => () => (
  <RequireAuth>
    <ScopeProvider>
      <AppShell>
        <SettingsLayout>
          <Inner />
        </SettingsLayout>
      </AppShell>
    </ScopeProvider>
  </RequireAuth>
);

const wrapAccount = (Inner: () => JSX.Element) => () => (
  <RequireAuth requirement="organization">
    <div class="min-h-dvh text-sm text-console-text">
      <AppBar leading={<BrandMark href="/" />} trailing={<ProfileMenu />} />
      <main class={ui.page}>
        <p class={ui.h3}>Your account</p>
        <div class="mt-3"><Inner /></div>
      </main>
    </div>
  </RequireAuth>
);

const wrapAdmin = (Inner: () => JSX.Element) => () => (
  <RequireAdmin>
    <AdminLayout>
      <Inner />
    </AdminLayout>
  </RequireAdmin>
);

export function App() {
  return (
    <Router>
      <Route path="/" component={wrap(Overview)} />
      <Route path="/login" component={Login} />
      <Route path="/invite" component={Invite} />
      <Route path="/auth/device" component={() => <RequireAuth requirement="organization"><Device /></RequireAuth>} />
      <Route path="/auth/slack/callback" component={AuthSlackCallback} />
      <Route path="/auth/slack/link" component={AuthSlackLink} />
      <Route path="/auth/slack/connect" component={SlackConnect} />
      <Route path="/auth/github/callback" component={AuthGitHubCallback} />
      <Route path="/auth/magic-link/callback" component={AuthMagicLinkCallback} />
      <Route path="/access-required" component={() => <RequireAuth requirement="session"><AccessRequired /></RequireAuth>} />
      <Route path="/organizations/new" component={() => <RequireAuth requirement="session"><OrganizationNew /></RequireAuth>} />

      <Route path="/agents" component={wrap(Agents)} />
      <Route path="/deployments/:deployment_id/agents/:agent" component={wrap(AgentDefinitionDetail)} />
      <Route path="/deployments/:deployment_id/templates/:template" component={wrap(TemplateDetail)} />
      <Route path="/agents/:agent" component={wrap(AgentDetail)} />
      <Route path="/templates" component={wrap(Templates)} />
      <Route path="/templates/:template" component={wrap(TemplateDetail)} />
      <Route path="/sessions" component={wrap(Sessions)} />
      <Route path="/sessions/:session_id" component={wrap(SessionDetail)} />
      <Route path="/sessions/:session_id/turns/:turn_id/asks/:ask_id" component={wrap(QuestionDetail)} />
      <Route path="/computers" component={wrap(Computers)} />
      <Route path="/computers/:computer_id" component={wrap(ComputerDetail)} />
      <Route path="/deployments" component={wrap(Deployments)} />
      <Route path="/deployments/:deployment_id" component={wrap(DeploymentDetail)} />
      <Route path="/projects/new" component={() => <RequireAuth requirement="organization"><ProjectNew /></RequireAuth>} />

      <Route path="/admin" component={() => <Navigate href="/admin/worker-groups" />} />
      <Route path="/admin/regions" component={wrapAdmin(AdminRegions)} />
      <Route path="/admin/worker-groups" component={wrapAdmin(AdminWorkerGroups)} />

      <Route path="/settings" component={() => <Navigate href="/settings/projects" />} />
      <Route path="/settings/projects" component={wrapSettings(Projects)} />
      <Route path="/settings/environments" component={wrapSettings(Environments)} />
      <Route path="/settings/members" component={wrapSettings(Members)} />
      <Route path="/settings/api-keys" component={wrapSettings(ApiKeys)} />
      <Route path="/settings/secrets" component={wrapSettings(Secrets)} />
      <Route path="/account/slack" component={wrapAccount(SlackSettings)} />
    </Router>
  );
}
