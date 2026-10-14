import type { ScopeIDs } from "./scope-link";

// Each tab keeps its own scope in sessionStorage. localStorage holds the last
// scope the user chose, which only seeds a tab that has none yet, and the
// Environment last used per Project, which picks the Environment when a tab
// switches Project. Neither is synchronized into an open tab.
const SCOPE_KEY = "helmr.scope";
const ENVIRONMENT_BY_PROJECT_KEY = "helmr.environment_by_project";

export type ScopeStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

// Storage can be unavailable (privacy modes, sandboxed frames) or throw on access.
export function browserStorage(kind: "localStorage" | "sessionStorage"): ScopeStorage | undefined {
  try {
    return window[kind];
  } catch {
    return undefined;
  }
}

function read(storage: ScopeStorage | undefined, key: string): unknown {
  try {
    const value = storage?.getItem(key);
    return value ? JSON.parse(value) : undefined;
  } catch {
    return undefined;
  }
}

function write(storage: ScopeStorage | undefined, key: string, value: unknown) {
  try {
    storage?.setItem(key, JSON.stringify(value));
  } catch {
    // A full or blocked storage only loses the remembered selection.
  }
}

function parseScope(value: unknown): ScopeIDs | undefined {
  if (!value || typeof value !== "object") return undefined;
  const { project, environment } = value as Record<string, unknown>;
  if (typeof project !== "string" || !project) return undefined;
  return { projectID: project, environmentID: typeof environment === "string" ? environment : "" };
}

export function readTabScope(tab: ScopeStorage | undefined, last: ScopeStorage | undefined): ScopeIDs {
  return parseScope(read(tab, SCOPE_KEY)) ?? parseScope(read(last, SCOPE_KEY)) ?? { projectID: "", environmentID: "" };
}

export function writeScope(storage: ScopeStorage | undefined, scope: ScopeIDs) {
  if (!scope.projectID) {
    try {
      storage?.removeItem(SCOPE_KEY);
    } catch {
      // See write.
    }
    return;
  }
  write(storage, SCOPE_KEY, { project: scope.projectID, environment: scope.environmentID });
}

export function readEnvironmentSelections(storage: ScopeStorage | undefined): Record<string, string> {
  const value = read(storage, ENVIRONMENT_BY_PROJECT_KEY);
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  return Object.fromEntries(
    Object.entries(value).filter((entry): entry is [string, string] => typeof entry[1] === "string"),
  );
}

export function writeEnvironmentSelections(storage: ScopeStorage | undefined, selections: Record<string, string>) {
  write(storage, ENVIRONMENT_BY_PROJECT_KEY, selections);
}
