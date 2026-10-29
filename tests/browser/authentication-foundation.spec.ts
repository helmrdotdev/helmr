import { expect, test } from "@playwright/test";

// The real local API and PostgreSQL verify credentials signed by Chromium's
// virtual authenticator. No authentication route is intercepted in this suite.
test("ordinary passkey registration, sign-in, replay rejection and operator isolation", async ({ page, context, baseURL }) => {
  const cdp = await context.newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: { protocol: "ctap2", transport: "usb", hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
  });
  try {
    await page.goto("/dev/login");
    const original = await (await page.request.get("/api/me")).json();
    const oldCookies = await context.cookies();
    expect(original.user_id).toBeTruthy();
    await page.goto("/settings/account");
    await page.getByLabel("Passkey name", { exact: true }).fill("Browser acceptance key");
    await page.getByRole("button", { name: "Add passkey", exact: true }).click();
    await expect(page.getByText("Passkey added.", { exact: true })).toBeVisible();
    const credentials = await cdp.send("WebAuthn.getCredentials", { authenticatorId });
    expect(credentials.credentials).toHaveLength(1);
    expect(credentials.credentials[0]?.isResidentCredential).toBe(true);
    expect(credentials.credentials[0]?.rpId).toBe("localhost");

    const logout = await page.request.post("/api/auth/logout", { headers: { Origin: baseURL! }, data: {} });
    expect(logout.status()).toBe(204);
    await context.addCookies(oldCookies); // A retained revoked cookie must not block fresh login.
    await page.goto("/login?next=%2Fsettings%2Faccount");
    let assertion: unknown;
    page.on("request", request => {
      if (new URL(request.url()).pathname === "/api/auth/passkey/verify") assertion = request.postDataJSON();
    });
    await page.getByRole("button", { name: "Continue with a passkey" }).click();
    await expect(page).toHaveURL(/\/settings\/account$/);
    const signedIn = await (await page.request.get("/api/me")).json();
    expect(signedIn.user_id).toBe(original.user_id);
    expect(assertion).toBeTruthy();
    const replay = await page.request.post("/api/auth/passkey/verify", { headers: { Origin: baseURL! }, data: assertion });
    expect(replay.ok()).toBe(false);

    await page.goto("/admin/login");
    await page.getByRole("button", { name: "Sign in with operator key" }).click();
    await expect(page.getByRole("alert")).toBeVisible();
    expect((await page.request.get("/admin/api/v1/me")).status()).toBe(403);
    expect((await (await page.request.get("/api/me")).json()).user_id).toBe(original.user_id);
  } finally {
    await cdp.send("WebAuthn.removeVirtualAuthenticator", { authenticatorId });
    await cdp.detach();
  }
});

test("deployment-local operator enrollment requires registration and a working assertion", async ({ page, context, baseURL }) => {
  test.skip(Boolean(process.env.HELMR_E2E_BASE_URL), "Enrollment is restricted to this suite's owned local database");
  const { execFileSync } = await import("node:child_process");
  const { mkdtempSync, readFileSync, rmSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const { join } = await import("node:path");
  const directory = mkdtempSync(join(tmpdir(), "helmr-operator-browser-"));
  const file = join(directory, "enrollment.json");
  const postgresOptions = readFileSync(".helmr-dev-e2e/postgres/postmaster.opts", "utf8");
  const socket = postgresOptions.match(/"-k" "([^"]+)"/)?.[1];
  const port = postgresOptions.match(/"-p" "([0-9]+)"/)?.[1];
  expect(socket).toBeTruthy(); expect(port).toBeTruthy();
  const cdp = await context.newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  const { authenticatorId } = await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: { protocol: "ctap2", transport: "usb", hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
  });
  try {
    execFileSync("go", ["run", "./cmd/control-plane", "operator", "enroll", "--label", "Browser acceptance operator", "--output-file", file], {
      env: { ...process.env, DATABASE_URL: `postgres://helmr_runtime@/postgres?host=${socket}&port=${port}&sslmode=disable` },
      stdio: "pipe", timeout: 60_000,
    });
    const enrollment = JSON.parse(readFileSync(file, "utf8"));
    await page.goto("/admin/login");
    await page.getByText("Set up or repair an operator key", { exact: true }).click();
    await page.getByLabel("Setup or repair capability", { exact: true }).fill(enrollment.capability);
    await page.getByRole("button", { name: "Continue", exact: true }).click();
    await page.getByRole("button", { name: "Register a key", exact: true }).click();
    await expect(page.getByRole("button", { name: "Verify this key", exact: true })).toBeVisible();
    expect((await page.request.get("/admin/api/v1/me")).status()).toBe(403);
    await page.getByRole("button", { name: "Verify this key", exact: true }).click();
    await expect(page.getByRole("status")).toHaveText("Your key is ready. Sign in with it above.");
    expect((await page.request.get("/admin/api/v1/me")).status()).toBe(403);
    await page.getByRole("button", { name: "Sign in with operator key", exact: true }).click();
    await expect(page).toHaveURL(/\/setup$/);
    expect((await page.request.get("/admin/api/v1/me")).status()).toBe(200);
    await expect(page.getByRole("heading", { name: "Setup is complete" })).toBeVisible();
    execFileSync("psql", ["-d", `postgres://helmr_runtime@/postgres?host=${socket}&port=${port}&sslmode=disable`, "-At", "-c", "UPDATE login_sessions SET issued_at=clock_timestamp()-interval '2 hours',authenticated_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()+interval '9 hours' WHERE channel='operator'"], { stdio: "pipe" });
    await page.goto("/admin/security");
    await page.getByRole("button", { name: "Reauthenticate and renew session", exact: true }).click();
    await expect(page.getByText("New 12-hour operator session started.", { exact: true })).toBeVisible();
    execFileSync("psql", ["-d", `postgres://helmr_runtime@/postgres?host=${socket}&port=${port}&sslmode=disable`, "-At", "-c", "UPDATE login_sessions SET issued_at=clock_timestamp()-interval '2 hours',authenticated_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()-interval '1 second' WHERE channel='operator'"], {
      env: { ...process.env, PGDATABASE: `postgres://helmr_runtime@/postgres?host=${socket}&port=${port}&sslmode=disable` }, stdio: "pipe",
    });
    await page.goto("/admin/security");
    await page.getByRole("link", { name: "Sign in as an operator", exact: true }).click();
    await page.getByRole("button", { name: "Sign in with operator key", exact: true }).click();
    await expect(page).toHaveURL(/\/admin\/security$/);
    expect((await page.request.get("/admin/api/v1/me")).status()).toBe(200);
    expect((await page.request.get("/api/me")).status()).toBe(403);
    await page.goto("/login");
    await page.getByRole("button", { name: "Continue with a passkey" }).click();
    await expect(page.getByRole("alert")).toBeVisible();
    expect((await page.request.get("/api/me")).status()).toBe(403);
    expect((await page.request.get("/admin/api/v1/me")).status()).toBe(200);

    // Supervised ordinary recovery installs the key at registration, with no
    // second assertion. Its old access is retired; its existing methods remain.
    const operator = await (await page.request.get("/admin/api/v1/me")).json();
    await page.goto("/dev/login");
    const account = await (await page.request.get("/api/me")).json();
    const priorKeys = await (await page.request.get("/api/account/credentials")).json();
    const repairFile = join(directory, "account-repair.json");
    execFileSync("go", ["run", "./cmd/control-plane", "operator", "repair-account", "--operator-id", operator.id, "--user-id", account.user_id, "--output-file", repairFile], {
      env: { ...process.env, DEPLOYMENT_MODE: "self-hosted", DATABASE_URL: `postgres://helmr_runtime@/postgres?host=${socket}&port=${port}&sslmode=disable` },
      stdio: "pipe", timeout: 60_000,
    });
    // The account holder uses their authenticator; the already authenticated
    // operator continues supervising through the independent operator cookie.
    await cdp.send("WebAuthn.clearCredentials", { authenticatorId });
    await page.goto("/admin/security");
    await page.getByLabel("Account repair capability", { exact: true }).fill(JSON.parse(readFileSync(repairFile, "utf8")).capability);
    await page.getByRole("button", { name: "Review repair target", exact: true }).click();
    let completion: { url: string; body: unknown } | undefined;
    page.on("request", request => {
      if (/\/account-repairs\/[^/]+\/complete$/.test(new URL(request.url()).pathname)) completion = { url: request.url(), body: request.postDataJSON() };
    });
    await page.getByRole("button", { name: "Account holder: register your key and complete repair", exact: true }).click();
    await expect(page.getByText("Account access restored. Existing access is revoked. The account holder can now sign in normally with their key.", { exact: true })).toBeVisible();
    expect((await page.request.get("/api/me")).status()).toBe(403);
    expect(completion).toBeTruthy();
    expect((await page.request.post(completion!.url, { data: completion!.body })).ok()).toBe(false);
    await page.goto("/login?next=%2Fsettings%2Faccount");
    await page.getByRole("button", { name: "Continue with a passkey", exact: true }).click();
    await expect(page).toHaveURL(/\/settings\/account$/);
    expect((await (await page.request.get("/api/me")).json()).user_id).toBe(account.user_id);
    const restoredKeys = await (await page.request.get("/api/account/credentials")).json();
    expect(restoredKeys.credentials.length).toBe(priorKeys.credentials.length + 1);
  } finally {
    rmSync(directory, { recursive: true, force: true });
    await cdp.send("WebAuthn.removeVirtualAuthenticator", { authenticatorId });
    await cdp.detach();
  }
});


test("real setup entry without an operator session renders its login action", async ({ page }) => {
  await page.goto("/setup");
  await expect(page.getByRole("heading", { name: "Set up Helmr" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Operator sign-in" })).toHaveAttribute("href", "/admin/login?next=%2Fsetup");
  await page.goto("/dev/login");
  await page.goto("/setup");
  await expect(page.getByRole("heading", { name: "Set up Helmr" })).toBeVisible();
});
