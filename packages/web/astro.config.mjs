import { defineConfig } from "astro/config";

export default defineConfig({
  site: "https://helmr.dev",
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
