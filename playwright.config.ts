import { defineConfig } from "@playwright/test";

const externalBaseURL = process.env.HELMR_E2E_BASE_URL;
const selfPort = Number.parseInt(
  process.env.HELMR_E2E_PORT ?? process.env.PASEO_PORT ?? "4173",
  10,
);
if (!Number.isInteger(selfPort) || selfPort < 1024 || selfPort > 65535) {
  throw new Error("HELMR_E2E_PORT must be an integer from 1024 through 65535");
}
const baseURL = externalBaseURL ?? `http://127.0.0.1:${selfPort}`;

export default defineConfig({
  testDir: "./tests/browser",
  fullyParallel: false,
  forbidOnly: Boolean(process.env.CI),
  retries: 0,
  workers: 1,
  reporter: "line",
  webServer: externalBaseURL
    ? undefined
    : {
        command:
          `PASEO_PORT=${selfPort} HELMR_DEV_CONSOLE_MODE=preview HELMR_DEV_CONSOLE_PORT=${selfPort} PUBLIC_URL=${baseURL} ./scripts/dev-console-stack.sh`,
        url: `${baseURL}/readyz`,
        reuseExistingServer: false,
        timeout: 120_000,
      },
  use: {
    baseURL,
    headless: true,
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  outputDir: "test-results",
});
