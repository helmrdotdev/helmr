import { getCollection, type CollectionEntry } from "astro:content";

export type DocEntry = CollectionEntry<"docs">;

export type DocGroup = {
  section: string;
  docs: DocEntry[];
};

export const getDocUrl = (doc: DocEntry) => `/docs/${doc.id}`;

export const getDocLabel = (doc: DocEntry) => doc.data.sidebarLabel ?? doc.data.title;

export const sortDocs = (docs: DocEntry[]) =>
  [...docs].sort((a, b) => {
    const orderDelta = a.data.order - b.data.order;
    if (orderDelta !== 0) return orderDelta;
    return a.data.title.localeCompare(b.data.title);
  });

export const getDocs = async () => {
  const docs = await getCollection("docs", ({ data }) => !data.draft);
  return sortDocs(docs);
};

export const groupDocs = (docs: DocEntry[]): DocGroup[] => {
  const groups = new Map<string, DocEntry[]>();

  for (const doc of docs) {
    const section = doc.data.section;
    groups.set(section, [...(groups.get(section) ?? []), doc]);
  }

  return Array.from(groups.entries()).map(([section, entries]) => ({
    section,
    docs: entries,
  }));
};

export const getAdjacentDocs = (docs: DocEntry[], currentId: string) => {
  const index = docs.findIndex((doc) => doc.id === currentId);

  return {
    previous: index > 0 ? docs[index - 1] : undefined,
    next: index >= 0 && index < docs.length - 1 ? docs[index + 1] : undefined,
  };
};
