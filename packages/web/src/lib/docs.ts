import { getCollection, type CollectionEntry } from "astro:content";
import { docsNav } from "./docs-nav";

export type DocEntry = CollectionEntry<"docs">;

export type ResolvedDocGroup = {
  label?: string;
  docs: DocEntry[];
};

export type ResolvedDocSection = {
  label: string;
  groups: ResolvedDocGroup[];
};

export const getDocUrl = (doc: DocEntry) => `/docs/${doc.id}`;

export const getDocLabel = (doc: DocEntry) => doc.data.sidebarLabel ?? doc.data.title;

const navIds: string[] = docsNav.flatMap((section) => section.groups.flatMap((group) => [...group.ids]));

const assertCompleteNavigation = (docs: DocEntry[]) => {
  const duplicateIds = navIds.filter((id, index) => navIds.indexOf(id) !== index);
  if (duplicateIds.length > 0) throw new Error(`Duplicate docs navigation IDs: ${[...new Set(duplicateIds)].join(", ")}`);

  const contentIds = new Set(docs.map((doc) => doc.id));
  const missing = navIds.filter((id) => !contentIds.has(id));
  const unlisted = docs.filter((doc) => !navIds.includes(doc.id)).map((doc) => doc.id);
  if (missing.length > 0 || unlisted.length > 0) {
    throw new Error(
      [`Docs navigation is out of sync.`, missing.length ? `Missing pages: ${missing.join(", ")}` : "", unlisted.length ? `Unlisted pages: ${unlisted.join(", ")}` : ""]
        .filter(Boolean)
        .join(" "),
    );
  }
};

export const getDocs = async () => {
  const docs = await getCollection("docs", ({ data }) => !data.draft);
  assertCompleteNavigation(docs);
  const byId = new Map(docs.map((doc) => [doc.id, doc]));
  return navIds.map((id) => byId.get(id)!);
};

export const groupDocs = (docs: DocEntry[]): ResolvedDocSection[] => {
  const byId = new Map(docs.map((doc) => [doc.id, doc]));
  return docsNav.map((section) => ({
    label: section.label,
    groups: section.groups.map((group) => ({
      label: "label" in group ? group.label : undefined,
      docs: group.ids.map((id) => byId.get(id)).filter((doc): doc is DocEntry => Boolean(doc)),
    })),
  }));
};

export const getDocNavigation = (doc: DocEntry) => {
  for (const section of docsNav) {
    for (const group of section.groups) {
      if ((group.ids as readonly string[]).includes(doc.id)) {
        return { section: section.label, group: "label" in group ? group.label : undefined };
      }
    }
  }
  throw new Error(`Doc is not present in navigation: ${doc.id}`);
};

export const getAdjacentDocs = (docs: DocEntry[], currentId: string) => {
  const index = docs.findIndex((doc) => doc.id === currentId);

  return {
    previous: index > 0 ? docs[index - 1] : undefined,
    next: index >= 0 && index < docs.length - 1 ? docs[index + 1] : undefined,
  };
};
