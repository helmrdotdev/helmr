import type { DocEntry } from "./docs";

export const SITE = {
  name: "Helmr",
  url: "https://helmr.dev",
  githubUrl: "https://github.com/helmrdotdev/helmr",
  defaultTitle: "Helmr — The durable runtime for AI agents",
  defaultDescription:
    "The durable runtime for AI agents. Each run is a Firecracker microVM checkpointed whole at approval points — pause, resume, schedule, replay in plain TypeScript. On our cloud or in your AWS.",
  defaultImage: "/og/helmr.png",
  locale: "en_US",
};

export type JsonLdNode = Record<string, unknown>;

export const absoluteUrl = (path: string, base = SITE.url) => new URL(path, base).toString();

const organizationId = `${SITE.url}/#organization`;
const websiteId = `${SITE.url}/#website`;
const softwareId = `${SITE.url}/#software`;

export const organizationJsonLd = (): JsonLdNode => ({
  "@type": "Organization",
  "@id": organizationId,
  name: SITE.name,
  url: SITE.url,
  logo: absoluteUrl(SITE.defaultImage),
  sameAs: [SITE.githubUrl],
  description: "The durable runtime for AI agents.",
});

export const websiteJsonLd = (): JsonLdNode => ({
  "@type": "WebSite",
  "@id": websiteId,
  name: SITE.name,
  url: SITE.url,
  publisher: { "@id": organizationId },
  inLanguage: "en",
});

export const softwareJsonLd = (): JsonLdNode => ({
  "@type": "SoftwareApplication",
  "@id": softwareId,
  name: SITE.name,
  applicationCategory: "DeveloperApplication",
  operatingSystem: "macOS, Linux",
  url: SITE.url,
  codeRepository: SITE.githubUrl,
  description: SITE.defaultDescription,
  publisher: { "@id": organizationId },
});

export const breadcrumbJsonLd = (items: Array<{ name: string; path: string }>): JsonLdNode => ({
  "@type": "BreadcrumbList",
  itemListElement: items.map((item, index) => ({
    "@type": "ListItem",
    position: index + 1,
    name: item.name,
    item: absoluteUrl(item.path),
  })),
});

export const docsCollectionJsonLd = (): JsonLdNode => ({
  "@type": "CollectionPage",
  "@id": `${absoluteUrl("/docs")}#webpage`,
  name: "Helmr Docs",
  description: "Documentation for installing, operating, and extending Helmr.",
  url: absoluteUrl("/docs"),
  isPartOf: { "@id": websiteId },
  publisher: { "@id": organizationId },
  inLanguage: "en",
});

export const docArticleJsonLd = (doc: DocEntry, path: string): JsonLdNode => {
  const url = absoluteUrl(path);

  return {
    "@type": "TechArticle",
    "@id": `${url}#article`,
    headline: doc.data.title,
    description: doc.data.description,
    url,
    mainEntityOfPage: {
      "@type": "WebPage",
      "@id": `${url}#webpage`,
    },
    isPartOf: { "@id": `${absoluteUrl("/docs")}#webpage` },
    author: { "@id": organizationId },
    publisher: { "@id": organizationId },
    about: doc.data.section,
    inLanguage: "en",
  };
};

export const jsonLdGraph = (nodes: JsonLdNode | JsonLdNode[]) => ({
  "@context": "https://schema.org",
  "@graph": Array.isArray(nodes) ? nodes : [nodes],
});
