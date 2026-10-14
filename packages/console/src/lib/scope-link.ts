// Console scope is ambient tab state. A URL carries it only as a one-time share
// hint: the tab adopts `project` and `environment`, then removes them.
export type ScopeIDs = { projectID: string; environmentID: string };

// A link a tab opened with: the scope it names (none when incomplete) and the
// scope the tab had before it.
export type ScopeLink = { hint: ScopeIDs | undefined; previous: ScopeIDs };

export type LinkNotice = "switched" | "unavailable";

const PROJECT_PARAM = "project";
const ENVIRONMENT_PARAM = "environment";

// A hint needs both IDs; a partial hint names no complete scope.
export function readScopeHint(search: string): ScopeIDs | undefined {
  const params = new URLSearchParams(search);
  const projectID = params.get(PROJECT_PARAM)?.trim() ?? "";
  const environmentID = params.get(ENVIRONMENT_PARAM)?.trim() ?? "";
  return projectID && environmentID ? { projectID, environmentID } : undefined;
}

// The same location without hint params, or undefined when there is nothing to remove.
export function withoutScopeHint(location: { pathname: string; search: string; hash: string }): string | undefined {
  const params = new URLSearchParams(location.search);
  if (!params.has(PROJECT_PARAM) && !params.has(ENVIRONMENT_PARAM)) return undefined;
  params.delete(PROJECT_PARAM);
  params.delete(ENVIRONMENT_PARAM);
  const search = params.toString();
  return `${location.pathname}${search ? `?${search}` : ""}${location.hash}`;
}

// An absolute link to the page that opens it in the given scope.
export function scopeShareURL(href: string, scope: ScopeIDs): string {
  const url = new URL(href);
  url.searchParams.set(PROJECT_PARAM, scope.projectID);
  url.searchParams.set(ENVIRONMENT_PARAM, scope.environmentID);
  return url.toString();
}

function sameScope(a: ScopeIDs, b: ScopeIDs): boolean {
  return a.projectID === b.projectID && a.environmentID === b.environmentID;
}

// When the linked Project cannot be opened, or has no such Environment, the tab
// returns to the scope it had before the link instead of an arbitrary one.
// `environments` lists the linked Project's Environments once its detail loaded.
export function linkRestoreScope(
  link: ScopeLink | undefined,
  state: { projectID: string; notFound: boolean; environments: string[] | undefined; rejectedProjectIDs: ReadonlySet<string> },
): ScopeIDs | undefined {
  const hint = link?.hint;
  const previous = link?.previous;
  if (!hint || !previous?.projectID || state.projectID !== hint.projectID) return undefined;
  const unavailable = state.notFound || (state.environments !== undefined && !state.environments.includes(hint.environmentID));
  if (!unavailable || state.rejectedProjectIDs.has(previous.projectID)) return undefined;
  if (state.notFound && previous.projectID === hint.projectID) return undefined;
  return previous;
}

// What the tab tells the user once its scope has settled after opening a link:
// a switch away from the scope it had, or that the link's scope is unavailable.
// A link to the scope the tab already had needs no notice.
export function linkNotice(link: ScopeLink | undefined, settled: ScopeIDs | undefined): LinkNotice | undefined {
  if (!link || !settled) return undefined;
  if (!link.hint || !sameScope(link.hint, settled)) return "unavailable";
  return link.previous.projectID && !sameScope(link.previous, link.hint) ? "switched" : undefined;
}
