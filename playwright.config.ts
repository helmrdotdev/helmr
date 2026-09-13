import { defineConfig } from "@playwright/test";
import path from "path";

const externalBaseURL = process.env.HELMR_E2E_BASE_URL;
const selfPort = Number.parseInt(
  process.env.HELMR_E2E_PORT ?? "4173",
  10,
);
if (!Number.isInteger(selfPort) || selfPort < 1024 || selfPort > 65535) {
  throw new Error("HELMR_E2E_PORT must be an integer from 1024 through 65535");
}

const e2eDevDir = path.join(process.cwd(), ".helmr-dev-e2e");
const baseURL = externalBaseURL ?? `http://127.0.0.1:${selfPort}`;

const managedWebServerEnv: Record<string, string> = {
  HELMR_DEV_DIR: e2eDevDir,
  HELMR_DEV_CONSOLE_MODE: "preview",
  HELMR_DEV_CONSOLE_PORT: String(selfPort),
  HELMR_DEV_CONTROL_PLANE_PORT: String(selfPort),
  HELMR_DEV_POSTGRES_PORT: String(selfPort + 1),
  HELMR_DEV_CLICKHOUSE_HTTP_PORT: String(selfPort + 2),
  PUBLIC_URL: `http://127.0.0.1:${selfPort}`,
};

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
        command: "./scripts/dev-e2e-stack.sh",
        env: managedWebServerEnv,
        url: `${baseURL}/readyz`,
        reuseExistingServer: false,
        timeout: 180_000,
        gracefulShutdown: {
          signal: "SIGTERM",
          timeout: 30_000,
        },
      },
  use: {
    baseURL,
    headless: true,
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  outputDir: "test-results",
});
