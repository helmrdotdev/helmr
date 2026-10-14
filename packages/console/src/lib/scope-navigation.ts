const COLLECTIONS = ["agents", "sessions", "triggers", "computers", "templates", "deployments"];

// Changing environment discards identities and filters belonging to the old one:
// a page below a collection returns to the collection. An Agent is addressed by
// its authored name in every environment, so its page and tab stay and show
// whether the new environment has it.
export function scopeSwitchPath(path: string): string {
  const [, section, resource] = path.split("/");
  if (section === "agents" && resource) return path;
  return COLLECTIONS.includes(section ?? "") ? `/${section}` : path;
}
