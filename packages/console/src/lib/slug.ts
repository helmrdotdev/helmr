const MAX_SLUG_LENGTH = 48;

// Derives an identifier (Project, Environment or organization ID) from a display
// name: lowercase ASCII letters, digits and single hyphens, at most 48 characters,
// never starting or ending with a hyphen.
export function slugify(value: string): string {
  return value
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9\s-]/g, "")
    .replace(/\s+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, MAX_SLUG_LENGTH)
    .replace(/^-|-$/g, "");
}
