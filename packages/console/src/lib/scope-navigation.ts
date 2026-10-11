// Changing environment discards identities and filters belonging to the old one.
export function scopeSwitchPath(path: string): string {
  const section = path.split("/")[1];
  return ["agents", "templates", "sessions", "computers", "deployments"].includes(section ?? "") ? `/${section}` : path;
}
