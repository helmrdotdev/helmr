import type { APIRoute, GetStaticPaths } from "astro";
import { getDocs, getDocUrl, type DocEntry } from "../../lib/docs";
import { SITE, absoluteUrl } from "../../lib/seo";

export const getStaticPaths: GetStaticPaths = async () => {
  const docs = await getDocs();
  return docs.map((doc) => ({
    params: { slug: doc.id },
    props: { doc },
  }));
};

export const GET: APIRoute = ({ props, site }) => {
  const doc = (props as { doc: DocEntry }).doc;
  const base = site?.toString() ?? SITE.url;
  const body = (doc as { body?: string }).body?.trim() ?? "";

  const text = `# ${doc.data.title}

URL: ${absoluteUrl(getDocUrl(doc), base)}
Description: ${doc.data.description}

${body}
`;

  return new Response(text, {
    headers: {
      "Content-Type": "text/markdown; charset=utf-8",
    },
  });
};
