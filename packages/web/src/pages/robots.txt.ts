import type { APIRoute } from "astro";
import { SITE, absoluteUrl } from "../lib/seo";

const aiSearchBots = ["OAI-SearchBot", "Claude-SearchBot", "PerplexityBot"];

const aiTrainingBots = [
  "GPTBot",
  "ClaudeBot",
  "Google-Extended",
  "Applebot-Extended",
  "Meta-ExternalAgent",
  "Amazonbot",
  "CCBot",
  "Bytespider",
  "cohere-training-data-crawler",
  "Diffbot",
  "ImagesiftBot",
  "YouBot",
];

const aiUserFetchers = [
  "ChatGPT-User",
  "Claude-User",
  "Perplexity-User",
  "MistralAI-User",
  "Meta-ExternalFetcher",
];

const allowGroup = (userAgent: string) => `User-agent: ${userAgent}\nAllow: /`;

export const GET: APIRoute = ({ site }) => {
  const base = site?.toString() ?? SITE.url;
  const body = [
    "User-agent: *",
    "Allow: /",
    "Content-Signal: ai-train=yes, search=yes, ai-input=yes",
    "",
    "# AI search and answer engines",
    aiSearchBots.map(allowGroup).join("\n\n"),
    "",
    "# AI training crawlers",
    aiTrainingBots.map(allowGroup).join("\n\n"),
    "",
    "# AI agents fetching on behalf of a user",
    aiUserFetchers.map(allowGroup).join("\n\n"),
    "",
    `Sitemap: ${absoluteUrl("/sitemap.xml", base)}`,
  ].join("\n");

  return new Response(`${body}\n`, {
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
    },
  });
};
