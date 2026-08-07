import { readFile } from "node:fs/promises";

const indexHtml = await readFile(new URL("../dist/index.html", import.meta.url), "utf8");
const textSizeRule = "<style>html{-webkit-text-size-adjust:100%;text-size-adjust:100%}</style>";

if (!indexHtml.includes(textSizeRule)) {
  throw new Error("built landing page is missing the Safari text-size adjustment rule");
}
