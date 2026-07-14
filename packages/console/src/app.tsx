import { A, Navigate, Route, Router, useLocation } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import type { JSX } from "solid-js";
import { createMemo, createSignal, onCleanup, onMount, Show } from "solid-js";
import { RequireAuth } from "./components/RequireAuth";
import { ScopeSwitcher } from "./components/ScopeSwitcher";
import { SettingsLayout } from "./components/SettingsLayout";
import { getMe, logout } from "./lib/auth";
import { ScopeProvider, useScope } from "./lib/scope";
import { cx, ui } from "./ui/styles";
import { Dashboard } from "./routes/dashboard";
import { Invite } from "./routes/invite";
import { Login } from "./routes/login";
import { AuthGitHubCallback } from "./routes/auth-github-callback";
import { AuthMagicLinkCallback } from "./routes/auth-magic-link-callback";
import { RunDetail } from "./routes/run-detail";
import { SessionDetail } from "./routes/session-detail";
import { Sessions } from "./routes/sessions";
import { Schedules } from "./routes/schedules";
import { Tasks } from "./routes/tasks";
import { ApiKeys } from "./routes/api-keys";
import { Secrets } from "./routes/secrets";
import { Members } from "./routes/members";
import { Projects } from "./routes/projects";
import { Environments } from "./routes/environments";
import { ProjectNew } from "./routes/project-new";
import { OrganizationNew } from "./routes/organization-new";
import { AccessRequired } from "./routes/access-required";
import { Device } from "./routes/device";
import { WorkspaceDetail } from "./routes/workspace-detail";
import { WorkspaceExecDetail } from "./routes/workspace-exec-detail";

function TabLink(props: {
  href: string;
  children: JSX.Element;
  matchPrefix?: boolean;
  activePrefix?: string;
}) {
  const location = useLocation();
  const active = () => {
    if (props.activePrefix) return location.pathname.startsWith(props.activePrefix);
    if (props.matchPrefix) return location.pathname === props.href || location.pathname.startsWith(`${props.href}/`);
    return location.pathname === props.href;
  };

  return (
    <A
      href={props.href}
      class={cx(ui.tabLink, active() ? ui.tabLinkActive : ui.tabLinkHover)}
    >
      {props.children}
    </A>
  );
}

function formatRole(role: string | null | undefined): string {
  if (!role) return "Member";
  return role
    .split(/[_\s-]+/)
    .filter(Boolean)
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1).toLowerCase())
    .join(" ");
}

function initialsFor(value: string): string {
  const parts = value.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0]!.slice(0, 2).toUpperCase();
  return `${parts[0]!.charAt(0)}${parts[parts.length - 1]!.charAt(0)}`.toUpperCase();
}

function ProfileMenu() {
  const me = createQuery(() => ({
    queryKey: ["me"],
    queryFn: getMe,
    retry: false,
    staleTime: 60_000,
  }));
  const [open, setOpen] = createSignal(false);
  const [imageFailed, setImageFailed] = createSignal(false);
  let wrapperRef: HTMLDivElement | undefined;

  const displayName = createMemo(() => me.data?.display_name?.trim() || me.data?.user_id || "Account");
  const role = createMemo(() => formatRole(me.data?.role));
  const imageURL = createMemo(() => me.data?.profile_image_url?.trim() || null);
  const showImage = createMemo(() => !!imageURL() && !imageFailed());
  const initials = createMemo(() => initialsFor(displayName()));

  onMount(() => {
    const onMouseDown = (event: MouseEvent) => {
      if (!wrapperRef?.contains(event.target as Node)) setOpen(false);
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onMouseDown);
    document.addEventListener("keydown", onKeyDown);
    onCleanup(() => {
      document.removeEventListener("mousedown", onMouseDown);
      document.removeEventListener("keydown", onKeyDown);
    });
  });

  return (
    <div class={"relative shrink-0"} ref={wrapperRef}>
      <button
        type="button"
        class={"grid size-7 cursor-pointer place-items-center overflow-hidden rounded-xs border border-console-border bg-[linear-gradient(to_bottom,#fbfcfd_0%,#eef1f3_100%)] p-0 font-mono text-[10px] font-medium leading-none text-console-muted transition duration-100 hover:border-console-border-strong hover:bg-[linear-gradient(to_bottom,#f3f5f7_0%,#dfe5ea_100%)] hover:text-console-text focus-visible:outline-2 focus-visible:outline-console-accent-soft data-[open=true]:border-console-border-strong data-[open=true]:bg-console-bg-panel data-[open=true]:text-console-text"}
        data-open={open() ? "true" : "false"}
        aria-haspopup="menu"
        aria-expanded={open()}
        aria-label="Open profile menu"
        onClick={() => setOpen((value) => !value)}
      >
        <Show
          when={showImage()}
          fallback={<span class={"select-none leading-none"}>{initials()}</span>}
        >
          <img
            src={imageURL()!}
            alt=""
            class={"size-full object-cover grayscale-[0.15]"}
            referrerpolicy="no-referrer"
            onError={() => setImageFailed(true)}
          />
        </Show>
      </button>

      <Show when={open()}>
        <div
          class={"absolute right-0 top-[calc(100%+7px)] z-50 w-56 overflow-hidden border border-console-border-strong bg-console-surface shadow-[2px_2px_0_rgb(15_23_42/0.12)]"}
          role="menu"
          aria-label="Profile"
        >
          <div class={"border-b border-console-border bg-console-bg-panel px-3 py-2.5"}>
            <div class={"truncate text-[12.5px] font-medium leading-tight text-console-text"}>{displayName()}</div>
            <div class={"mt-0.75 font-mono text-[10.5px] font-medium uppercase tracking-[0.06em] text-console-subtle"}>{role()}</div>
          </div>
          <div class={"p-1"}>
            <button
              type="button"
              class={cx(ui.scopeAction, "text-console-danger hover:text-console-danger")}
              role="menuitem"
              onClick={() => {
                setOpen(false);
                void logout();
              }}
            >
              Sign out
            </button>
          </div>
        </div>
      </Show>
    </div>
  );
}

function AppShell(props: { children?: JSX.Element }) {
  const scope = useScope();
  return (
    <div class={"min-h-dvh bg-transparent font-sans text-sm text-console-text antialiased"}>
      <header class={"sticky top-0 z-30 flex h-10 items-center gap-3 border-b border-console-border-strong bg-[linear-gradient(to_bottom,#fafafa_0%,#eceff2_100%)] px-5 max-[860px]:px-3"}>
        <ScopeSwitcher />
        <span class={"h-5 w-px shrink-0 bg-console-border"} aria-hidden="true" />
        <nav class={"flex min-w-0 flex-1 items-center gap-1 overflow-x-auto px-1 scrollbar-none [&::-webkit-scrollbar]:hidden"} aria-label="Sections">
          <TabLink href="/">Dashboard</TabLink>
          <TabLink href="/tasks">Tasks</TabLink>
          <TabLink href="/sessions" matchPrefix>Sessions</TabLink>
          <TabLink href="/schedules">Schedules</TabLink>
          <TabLink href="/settings/projects" activePrefix="/settings">Settings</TabLink>
        </nav>
        <div class={"flex items-center gap-2"}>
          <ProfileMenu />
        </div>
      </header>
      <Show when={scope.error()}>
        <div class={"border-b border-[#e6aaa4] bg-[#fff1ef] px-5 py-2 text-[13px] font-medium text-console-danger"} role="alert">Could not load projects.</div>
      </Show>
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

export function App() {
  return (
    <Router>
      <Route path="/" component={wrap(Dashboard)} />
      <Route path="/login" component={Login} />
      <Route path="/invite" component={Invite} />
      <Route path="/auth/device" component={() => <RequireAuth><Device /></RequireAuth>} />
      <Route path="/auth/github/callback" component={AuthGitHubCallback} />
      <Route path="/auth/magic-link/callback" component={AuthMagicLinkCallback} />
      <Route path="/access-required" component={() => <RequireAuth allowOnboarding><AccessRequired /></RequireAuth>} />
      <Route path="/organizations/new" component={() => <RequireAuth allowOnboarding><OrganizationNew /></RequireAuth>} />

      <Route path="/sessions" component={wrap(Sessions)} />
      <Route path="/sessions/:session_id/runs/:run_id" component={wrap(RunDetail)} />
      <Route path="/sessions/:id" component={wrap(SessionDetail)} />
      <Route path="/schedules" component={wrap(Schedules)} />
      <Route path="/tasks" component={wrap(Tasks)} />
      <Route path="/workspaces/:workspace_id" component={wrap(WorkspaceDetail)} />
      <Route path="/workspaces/:workspace_id/execs/:exec_id" component={wrap(WorkspaceExecDetail)} />
      <Route path="/projects/new" component={() => <RequireAuth allowOnboarding><ProjectNew /></RequireAuth>} />

      <Route path="/settings" component={() => <Navigate href="/settings/projects" />} />
      <Route path="/settings/projects" component={wrapSettings(Projects)} />
      <Route path="/settings/environments" component={wrapSettings(Environments)} />
      <Route path="/settings/members" component={wrapSettings(Members)} />
      <Route path="/settings/api-keys" component={wrapSettings(ApiKeys)} />
      <Route path="/settings/secrets" component={wrapSettings(Secrets)} />
    </Router>
  );
}
