import { defineConfig } from "astro/config";

export default defineConfig({
  site: "https://helmr.dev",
  markdown: {
    shikiConfig: {
      theme: "github-light",
    },
  },
});
