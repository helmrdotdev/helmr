import type { APIRoute } from "astro";
import { getDocs, getDocUrl } from "../lib/docs";
import { SITE, absoluteUrl } from "../lib/seo";

export const GET: APIRoute = async ({ site }) => {
  const base = site?.toString() ?? SITE.url;
  const docs = await getDocs();
  const sections = docs
    .map((doc) => {
      const body = (doc as { body?: string }).body?.trim() ?? "";
      return `# ${doc.data.title}

URL: ${absoluteUrl(getDocUrl(doc), base)}
Section: ${doc.data.section}
Description: ${doc.data.description}

${body}`;
    })
    .join("\n\n---\n\n");

  const body = `# Helmr Full Documentation

Build your own coding agent runtime. This file is generated from the public Helmr documentation corpus for AI assistants and search systems that prefer consolidated plain text.

Official site: ${absoluteUrl("/", base)}
Documentation: ${absoluteUrl("/docs", base)}
Source code: ${SITE.githubUrl}

---

${sections}
`;

  return new Response(body, {
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
    },
  });
};
