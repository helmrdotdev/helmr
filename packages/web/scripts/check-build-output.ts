import { access, readFile, readdir } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const packageRoot = dirname(fileURLToPath(new URL("../package.json", import.meta.url)));
const distRoot = join(packageRoot, "dist");

const indexHtml = await readFile(new URL("../dist/index.html", import.meta.url), "utf8");
const textSizeRule = "<style>html{-webkit-text-size-adjust:100%;text-size-adjust:100%}</style>";

if (!indexHtml.includes(textSizeRule)) {
  throw new Error("built landing page is missing the Safari text-size adjustment rule");
}

const files = await readdir(distRoot, { recursive: true });
const htmlFiles = files.filter((file) => file.endsWith(".html"));

const routeExists = async (pathname: string) => {
  const normalized = decodeURI(pathname).replace(/^\/+|\/+$/g, "");
  const candidates = normalized.length === 0
    ? [join(distRoot, "index.html")]
    : [join(distRoot, normalized, "index.html"), join(distRoot, `${normalized}.html`), join(distRoot, normalized)];
  for (const candidate of candidates) {
    try {
      await access(candidate);
      return true;
    } catch {
      // Try the next static output shape.
    }
  }
  return false;
};

// Canonical form is a clean URL without a trailing slash, or a .md asset.
const brokenDocsLinks: string[] = [];
const nonCanonicalLinks: string[] = [];
const nonCanonicalUrls: string[] = [];
for (const file of htmlFiles) {
  const html = await readFile(join(distRoot, file), "utf8");
  for (const match of html.matchAll(/href="(\/docs(?:\/[^"#?]*)?)(?:[?#][^"]*)?"/g)) {
    const path = match[1];
    if (path.endsWith("/")) nonCanonicalLinks.push(`${file}: ${path}`);
    if (!(await routeExists(path))) brokenDocsLinks.push(`${file}: ${path}`);
  }
  for (const match of html.matchAll(/<link rel="canonical" href="([^"]+)"/g)) {
    const pathname = new URL(match[1]).pathname;
    if (pathname !== "/" && pathname.endsWith("/")) nonCanonicalUrls.push(`${file}: ${match[1]}`);
  }
}
if (brokenDocsLinks.length > 0) {
  throw new Error(`built pages contain unresolved docs links:\n${brokenDocsLinks.join("\n")}`);
}
if (nonCanonicalLinks.length > 0) {
  throw new Error(`built pages contain non-canonical docs links (must not end in "/"):\n${nonCanonicalLinks.join("\n")}`);
}
if (nonCanonicalUrls.length > 0) {
  throw new Error(`built pages declare non-canonical URLs (must not end in "/"):\n${nonCanonicalUrls.join("\n")}`);
}

const sitemap = await readFile(join(distRoot, "sitemap.xml"), "utf8");
const retiredPaths = ["/docs/start/", "/docs/reference/sdk-authoring", "/docs/reference/runtime-client"];
for (const path of retiredPaths) {
  if (sitemap.includes(path)) throw new Error(`sitemap contains retired docs path: ${path}`);
}

const docsSourceRoot = join(packageRoot, "src/content/docs");
const markdownFiles = (await readdir(docsSourceRoot, { recursive: true })).filter((file) => file.endsWith(".md"));
const allowedFrontmatterFields = new Set(["title", "description", "sidebarLabel", "draft"]);
for (const file of markdownFiles) {
  const source = await readFile(join(docsSourceRoot, file), "utf8");
  const frontmatter = source.match(/^---\n([\s\S]*?)\n---/)?.[1] ?? "";
  const fields = [...frontmatter.matchAll(/^([A-Za-z][\w-]*):/gm)].map((match) => match[1]);
  const unexpectedFields = fields.filter((field) => !allowedFrontmatterFields.has(field));
  if (unexpectedFields.length > 0) {
    throw new Error(`unsupported docs frontmatter in ${file}: ${unexpectedFields.join(", ")}`);
  }
}
