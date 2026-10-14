// One entry of a CommandList: a label, the words it is also found by, and what
// choosing it does.
export type CommandEntry = {
  id: string;
  label: string;
  group?: string | undefined;
  keywords?: string | undefined;
};

// Whether every word of the query appears in the entry's label or keywords.
export function matchesQuery(entry: CommandEntry, query: string): boolean {
  const words = query.trim().toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return true;
  const text = `${entry.label} ${entry.keywords ?? ""}`.toLowerCase();
  return words.every((word) => text.includes(word));
}

// Entries in their given order, grouped under each group's first appearance,
// with each entry's position in the flat list for keyboard navigation.
export function groupEntries<T extends CommandEntry>(entries: readonly T[]): { label: string | undefined; entries: { entry: T; index: number }[] }[] {
  const groups: { label: string | undefined; entries: { entry: T; index: number }[] }[] = [];
  const byLabel = new Map<string | undefined, (typeof groups)[number]>();
  entries.forEach((entry, index) => {
    let group = byLabel.get(entry.group);
    if (!group) {
      group = { label: entry.group, entries: [] };
      byLabel.set(entry.group, group);
      groups.push(group);
    }
    group.entries.push({ entry, index });
  });
  // Positions follow the grouped order, which is the order the list shows.
  let position = 0;
  for (const group of groups) for (const item of group.entries) item.index = position++;
  return groups;
}

// The key a list's search box handles, or undefined for keys it leaves alone.
// Home and End move to the first and last item only while the box is empty;
// with a query they keep moving the text cursor. Nothing is handled while an
// input method is composing text.
export function listKey(event: Pick<KeyboardEvent, "key" | "isComposing" | "keyCode">, query: string): "next" | "previous" | "first" | "last" | "choose" | undefined {
  if (event.isComposing || event.keyCode === 229) return undefined;
  switch (event.key) {
    case "ArrowDown": return "next";
    case "ArrowUp": return "previous";
    case "Home": return query === "" ? "first" : undefined;
    case "End": return query === "" ? "last" : undefined;
    case "Enter": return "choose";
  }
  return undefined;
}
