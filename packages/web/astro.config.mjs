import { defineConfig } from "astro/config";

export default defineConfig({
  site: "https://helmr.com",
  markdown: {
    shikiConfig: {
      theme: "github-light",
    },
  },
});
