import { defineConfig } from "astro/config";

export default defineConfig({
  site: "https://helmr.dev",
  trailingSlash: "never",
  build: {
    format: "file",
  },
  prefetch: {
    prefetchAll: false,
    defaultStrategy: "hover",
  },
  markdown: {
    shikiConfig: {
      theme: "css-variables",
    },
  },
});
