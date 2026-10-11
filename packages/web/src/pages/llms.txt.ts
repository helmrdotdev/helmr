import type { APIRoute } from "astro";
import { getDocs, getDocMarkdownUrl, groupDocs } from "../lib/docs";
import { harnesses, interfaces, positioning, usecases } from "../lib/messaging";
import { SITE, absoluteUrl } from "../lib/seo";

export const GET: APIRoute = async ({ site }) => {
  const base = site?.toString() ?? SITE.url;
  const docs = await getDocs();
  const groups = groupDocs(docs);

  const docLines = groups
    .map((section) => {
      const links = section.groups
        .map((group) => {
          const heading = group.label ? `### ${group.label}\n` : "";
          const items = group.docs
            .map((doc) => `- [${doc.data.title}](${absoluteUrl(getDocMarkdownUrl(doc), base)}): ${doc.data.description}`)
            .join("\n");
          return `${heading}${items}`;
        })
        .join("\n\n");
      return `## ${section.label}\n${links}`;
    })
    .join("\n\n");

  const primitives = [...new Set(usecases.map((usecase) => usecase.primitive))].join(", ");
  const agents = harnesses.map((harness) => harness.name).join(", ");
  const surfaces = interfaces.map((iface) => iface.name).join(", ");

  const body = `# Helmr

> ${SITE.tagline}. ${SITE.defaultDescription}

Official site: ${absoluteUrl("/", base)}
Documentation: ${absoluteUrl("/docs", base)}
Source code: ${SITE.githubUrl}
License: Apache 2.0

## Product Context
- ${positioning}
- Application sketches: ${primitives}; harness examples: ${agents}; client entry points: ${surfaces}. Local adapters require application configuration.
- Self-hosted in your own AWS account. Managed Helmr Cloud is planned and is not currently available.
- Every docs page is also served as Markdown by appending .md to the page path.

## Core Pages
- [Home](${absoluteUrl("/", base)}): ${SITE.tagline} — infrastructure and APIs for your own agent harness.
- [Docs](${absoluteUrl("/docs", base)}): Documentation index for installing, operating, and extending Helmr.

${docLines}

## Full Text
- [llms-full.txt](${absoluteUrl("/llms-full.txt", base)}): Expanded Markdown-oriented corpus for language models.
`;

  return new Response(body, {
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
    },
  });
};
