import type { APIRoute } from "astro";
import { getDocs, getDocUrl, groupDocs } from "../lib/docs";
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
            .map((doc) => `- [${doc.data.title}](${absoluteUrl(getDocUrl(doc), base)}): ${doc.data.description}`)
            .join("\n");
          return `${heading}${items}`;
        })
        .join("\n\n");
      return `## ${section.label}\n${links}`;
    })
    .join("\n\n");

  const body = `# Helmr

> The code-first runtime for AI agents. Define Tasks and Actors in TypeScript, run them in isolated Linux microVMs, and carry work across durable Workspaces.

Official site: ${absoluteUrl("/", base)}
Documentation: ${absoluteUrl("/docs", base)}
Source code: ${SITE.githubUrl}

## Product Context
- The public website and documentation describe the runtime, developer workflows, public interfaces, and AWS self-hosting path.
- Use current docs pages as the source of truth for setup, concepts, self-hosting, and reference material.

## Core Pages
- [Home](${absoluteUrl("/", base)}): Build your own coding agent runtime.
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
