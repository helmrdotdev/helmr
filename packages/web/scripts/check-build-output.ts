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
    : [join(distRoot, normalized, "index.html"), join(distRoot, `${normalized}.html`)];
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

const brokenDocsLinks: string[] = [];
for (const file of htmlFiles) {
  const html = await readFile(join(distRoot, file), "utf8");
  for (const match of html.matchAll(/href="(\/docs(?:\/[^"#?]*)?)(?:[?#][^"]*)?"/g)) {
    if (!(await routeExists(match[1]))) brokenDocsLinks.push(`${file}: ${match[1]}`);
  }
}
if (brokenDocsLinks.length > 0) {
  throw new Error(`built pages contain unresolved docs links:\n${brokenDocsLinks.join("\n")}`);
}

const redirects = await readFile(join(distRoot, "_redirects"), "utf8");
for (const line of redirects.split("\n")) {
  const trimmed = line.trim();
  if (trimmed.length === 0 || trimmed.startsWith("#")) continue;
  const [, target, status] = trimmed.split(/\s+/);
  if (status !== "301") throw new Error(`docs redirect is not permanent: ${trimmed}`);
  if (!target || !(await routeExists(target))) throw new Error(`docs redirect target does not exist: ${trimmed}`);
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
