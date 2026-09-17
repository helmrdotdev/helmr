import type { APIRoute } from "astro";
import { getDocs, getDocLastmod, getDocUrl, gitLastmod } from "../lib/docs";
import { SITE, absoluteUrl } from "../lib/seo";

const escapeXml = (value: string) =>
  value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&apos;");

export const GET: APIRoute = async ({ site }) => {
  const base = site?.toString() ?? SITE.url;
  const docs = await getDocs();
  const routes = [
    { path: "/", changefreq: "weekly", priority: "1.0", lastmod: gitLastmod("src/pages/index.astro") },
    { path: "/docs", changefreq: "weekly", priority: "0.9", lastmod: gitLastmod("src/pages/docs/index.astro") },
    ...docs.map((doc) => ({
      path: getDocUrl(doc),
      changefreq: "weekly",
      priority: "0.7",
      lastmod: getDocLastmod(doc),
    })),
  ];

  const urls = routes
    .map(
      (route) => `  <url>
    <loc>${escapeXml(absoluteUrl(route.path, base))}</loc>
${route.lastmod ? `    <lastmod>${route.lastmod}</lastmod>\n` : ""}    <changefreq>${route.changefreq}</changefreq>
    <priority>${route.priority}</priority>
  </url>`,
    )
    .join("\n");

  return new Response(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
${urls}
</urlset>
`, {
    headers: {
      "Content-Type": "application/xml; charset=utf-8",
    },
  });
};
