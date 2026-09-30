import { defineConfig } from "cf/config";

export default defineConfig({
  worker: {
    name: "helmr-web",
    compatibilityDate: "2026-05-18",
    assets: {
      htmlHandling: "auto-trailing-slash",
    },
    domains: ["helmr.dev"],
  },
});
