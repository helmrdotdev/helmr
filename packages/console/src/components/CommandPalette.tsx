import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { useLocation, useNavigate } from "@solidjs/router";
import { createMemo, createSignal, onCleanup, onMount, Show } from "solid-js";
import { environmentChip } from "../features/projects/display";
import { agentHref, listRetainedAgents } from "../lib/definitions";
import { useMe } from "../lib/me";
import { isProtectedEnvironment, type Project } from "../lib/projects";
import { useScope, useScopeIDs } from "../lib/scope";
import { isApplePlatform, isPaletteShortcut } from "../lib/shortcuts";
import { scopeSwitchPath } from "../lib/scope-navigation";
import { listSessions, sessionConsolePath } from "../lib/sessions";
import { CommandList, type CommandItem } from "../ui/CommandList";
import { Dialog } from "../ui/Dialog";
import { EnvDot } from "../ui/EnvChip";
import { formatID } from "../ui/id";
import { cx } from "../ui/styles";

const PAGES: readonly { label: string; href: string; keywords?: string }[] = [
  { label: "Overview", href: "/", keywords: "needs you questions holds" },
  { label: "Agents", href: "/agents" },
  { label: "Sessions", href: "/sessions" },
  { label: "Triggers", href: "/triggers" },
  { label: "Computers", href: "/computers" },
  { label: "Templates", href: "/templates" },
  { label: "Deployments", href: "/deployments" },
  { label: "Project settings", href: "/settings/project", keywords: "settings general history retention" },
  { label: "Environments", href: "/settings/environments", keywords: "settings" },
  { label: "Secrets", href: "/settings/secrets", keywords: "settings" },
  { label: "API keys", href: "/settings/api-keys", keywords: "settings tokens" },
  { label: "Members", href: "/settings/members", keywords: "settings organization invitations" },
  { label: "Account settings", href: "/settings/account", keywords: "settings profile slack" },
];

// Agents and Sessions show this many items until a query narrows them.
const UNFILTERED = 5;
const FILTERED = 12;


function SearchIcon() {
  return (
    <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
      <circle cx="11" cy="11" r="7" />
      <path d="m20 20-3.5-3.5" />
    </svg>
  );
}

function Palette(props: { onClose: () => void }) {
  const scope = useScope();
  const ids = useScopeIDs();
  const me = useMe();
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const enabled = () => !!ids().projectID && !!ids().environmentID;
  // One bounded request each while the palette is open.
  const agents = createQuery(() => ({
    queryKey: ["agents", "palette", ids().projectID, ids().environmentID],
    queryFn: () => listRetainedAgents(ids()),
    enabled: enabled(),
    retry: false,
    staleTime: 30_000,
  }));
  const sessions = createQuery(() => ({
    queryKey: ["sessions", "palette", ids().projectID, ids().environmentID],
    queryFn: () => listSessions({ ...ids(), limit: 20 }),
    enabled: enabled(),
    retry: false,
    staleTime: 10_000,
  }));

  const go = (href: string) => {
    props.onClose();
    navigate(href);
  };
  const switchTo = (projectID: string, environmentID: string | undefined) => {
    props.onClose();
    if (projectID !== scope.selectedProjectID()) scope.setSelectedProjectID(projectID);
    if (environmentID) scope.setSelectedEnvironmentID(environmentID);
    navigate(scopeSwitchPath(location.pathname));
  };

  // Other Projects list their Environments when their details are already
  // loaded; otherwise the Project opens in the Environment it last used.
  const knownProject = (project: Project): Project | undefined =>
    queryClient.getQueryData<{ projectID: string; project: Project | undefined }>(["projects", "detail", project.project])?.project;

  const items = createMemo((): CommandItem[] => {
    const current = scope.selectedProject();
    const switchGroup = "Switch environment";
    const environments: CommandItem[] = (current?.environments ?? []).map((environment) => ({
      id: `environment:${current!.project}/${environment.environment}`,
      group: switchGroup,
      label: environment.name,
      keywords: `${environment.environment} ${current!.name} environment`,
      leading: <EnvDot colorHex={environmentChip(environment)?.colorHex} />,
      trailing: isProtectedEnvironment(environment) ? <span class="font-mono text-micro text-console-accent">protected</span> : undefined,
      current: environment.environment === scope.selectedEnvironmentID(),
      onSelect: () => switchTo(current!.project, environment.environment),
    }));
    for (const project of scope.projects()) {
      if (project.project === current?.project) continue;
      const detail = knownProject(project);
      if (detail?.environments?.length) {
        for (const environment of detail.environments) {
          environments.push({
            id: `environment:${project.project}/${environment.environment}`,
            group: switchGroup,
            label: `${project.name} / ${environment.name}`,
            keywords: `${project.project} ${environment.environment} project environment`,
            leading: <EnvDot colorHex={environmentChip(environment)?.colorHex} />,
            onSelect: () => switchTo(project.project, environment.environment),
          });
        }
      } else {
        environments.push({
          id: `project:${project.project}`,
          group: switchGroup,
          label: project.name,
          keywords: `${project.project} project`,
          trailing: <span class="font-mono text-micro text-console-subtle">last used environment</span>,
          onSelect: () => switchTo(project.project, undefined),
        });
      }
    }
    const pages: CommandItem[] = PAGES.map((page) => ({
      id: `page:${page.href}`,
      group: "Go to",
      label: page.label,
      keywords: page.keywords,
      current: location.pathname === page.href,
      onSelect: () => go(page.href),
    }));
    if (me.data?.admin) pages.push({ id: "page:/admin", group: "Go to", label: "Admin", onSelect: () => go("/admin") });
    const agentItems: CommandItem[] = (agents.data?.agents ?? []).map((agent) => ({
      id: `agent:${agent.agent}`,
      group: "Agents",
      label: agent.agent,
      keywords: "agent",
      onSelect: () => go(agentHref(agent.agent)),
    }));
    const sessionItems: CommandItem[] = (sessions.data?.sessions ?? []).map((session) => ({
      id: `session:${session.id}`,
      group: "Sessions",
      label: `${formatID(session.id)} · ${session.agent}`,
      keywords: `${session.id} session ${session.status}`,
      onSelect: () => go(sessionConsolePath(session.id)),
    }));
    return [...environments, ...pages, ...agentItems, ...sessionItems];
  });

  // Focus the search box as the palette opens, so typing right after the
  // shortcut lands in it.
  let input: HTMLInputElement | undefined;
  onMount(() => input?.focus());
  return (
    <Dialog title="Command palette" onClose={props.onClose} class="max-w-140" bodyClass="p-0">
      <CommandList
        label="Results"
        searchLabel="Search"
        placeholder="Switch environment, go to a page, find an Agent or Session…"
        empty="Nothing matches."
        items={items()}
        inputRef={(element) => { input = element; }}
        class="max-h-[min(60vh,440px)]"
        limitPerGroup={(query) => (query ? FILTERED : UNFILTERED)}
      />
    </Dialog>
  );
}

// The header's Search button and the palette it opens, also on Cmd/Ctrl+K.
export function CommandPalette() {
  const [open, setOpen] = createSignal(false);
  const apple = isApplePlatform();
  onMount(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (!isPaletteShortcut(event, apple)) return;
      // The shortcut closes the palette, but never opens it over another modal.
      if (!open() && document.querySelector("[aria-modal='true']")) return;
      event.preventDefault();
      setOpen((value) => !value);
    };
    window.addEventListener("keydown", onKeyDown);
    onCleanup(() => window.removeEventListener("keydown", onKeyDown));
  });
  const shortcut = apple ? "⌘K" : "Ctrl K";
  return (
    <>
      <button
        type="button"
        class={cx(
          "flex h-6.5 shrink-0 cursor-pointer items-center gap-2 rounded-xs border border-console-border bg-console-surface px-2 font-mono text-chrome text-console-muted transition hover:border-console-border-strong hover:text-console-text",
          "max-[720px]:size-7 max-[720px]:justify-center max-[720px]:px-0",
        )}
        aria-label="Search"
        aria-keyshortcuts={apple ? "Meta+K" : "Control+K"}
        aria-haspopup="dialog"
        onClick={() => setOpen(true)}
      >
        <SearchIcon />
        <span class="max-[720px]:hidden">Search</span>
        <kbd class="rounded-xs border border-console-border px-1 font-mono text-micro leading-normal text-console-subtle max-[720px]:hidden">{shortcut}</kbd>
      </button>
      <Show when={open()}>
        <Palette onClose={() => setOpen(false)} />
      </Show>
    </>
  );
}
