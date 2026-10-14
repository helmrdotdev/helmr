// Changing environment discards identities and filters belonging to the old one.
// An Agent is addressed by its authored name in every environment, so its page
// stays and shows whether the new environment has it.
export function scopeSwitchPath(path: string): string {
  const [, section, resource, ...rest] = path.split("/");
  if (section === "agents" && resource && rest.length === 0) return path;
  return ["agents", "templates", "sessions", "computers", "deployments"].includes(section ?? "") ? `/${section}` : path;
}
