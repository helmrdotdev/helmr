import { expect, test, type BrowserContext } from "@playwright/test";

async function setupFixture(context: BrowserContext, initial: { operator: boolean; account: boolean; complete?: boolean; failure?: string }) {
  const state = { ...initial };
  await context.route("**/api/**", async route => {
    const path = new URL(route.request().url()).pathname;
    const json = (value: unknown, status = 200) => route.fulfill({ status, json: value });
    const missing = () => json({ error: { code: "authentication_required", message: "Sign in to continue." } }, 403);
    if (path === state.failure) return json({ error: { code: "internal_error", message: "Unavailable" } }, 500);
    if (path === "/admin/api/v1/me") return state.operator ? json({ id: "operator" }) : missing();
    if (path === "/admin/api/v1/setup") return state.operator ? json({ owner_setup_required: !state.complete }) : missing();
    if (path === "/api/me") return state.account ? json({ user_id: "owner", display_name: "First Owner", primary_email: "owner@example.test", profile_image_url: "" }) : missing();
    if (path === "/api/auth/config") return json({ deployment_mode: "self-hosted", methods: ["email", "passkey"], setup_required: true, organization_creation_enabled: false, email_delivery: true });
    if (path === "/api/organizations") return json({ organizations: [] });
    return json({ error: { code: "not_found", message: path } }, 404);
  });
  return state;
}

for (const account of [false, true]) {
  test(`setup without operator shows sign-in, ordinary account=${account}`, async ({ page, context }) => {
    await setupFixture(context, { operator: false, account });
    await page.goto("/setup");
    await expect(page.getByRole("heading", { name: "Set up Helmr" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Operator sign-in" })).toHaveAttribute("href", "/admin/login?next=%2Fsetup");
    await page.getByRole("link", { name: "Operator sign-in" }).click();
    await expect(page.getByRole("heading", { name: "Operator sign-in", exact: true })).toBeVisible();
  });
}

test("operator alone is asked to sign into the owner account", async ({ page, context }) => {
  await setupFixture(context, { operator: true, account: false });
  await page.goto("/setup");
  await expect(page.getByRole("heading", { name: "Choose the first owner" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Sign in to the owner account" })).toHaveAttribute("href", "/login?next=%2Fsetup");
});

test("both identities reach organization creation", async ({ page, context }) => {
  await setupFixture(context, { operator: true, account: true });
  await page.goto("/setup");
  await expect(page.getByRole("heading", { name: "Create your organization" })).toBeVisible();
  await expect(page.getByText("First Owner", { exact: true })).toBeVisible();
});

test("completed setup does not require an ordinary login", async ({ page, context }) => {
  await setupFixture(context, { operator: true, account: false, complete: true });
  await page.goto("/setup");
  await expect(page.getByRole("heading", { name: "Setup is complete" })).toBeVisible();
});

for (const failure of ["/admin/api/v1/setup", "/api/me"]) {
  test(`setup handles and retries ${failure} server failures`, async ({ page, context }) => {
    const state = await setupFixture(context, { operator: true, account: false, failure });
    await page.goto("/setup");
    await expect(page.getByRole("alert")).toContainText("Unable to check");
    await expect(page.getByRole("link", { name: "Operator sign-in", exact: true })).toHaveCount(0);
    delete state.failure;
    await page.getByRole("button", { name: "Retry", exact: true }).click();
    await expect(page.getByRole("heading", { name: "Choose the first owner" })).toBeVisible();
  });
}

test("protected admin entry preserves its destination at the login link", async ({ page, context }) => {
  await setupFixture(context, { operator: false, account: true });
  await page.goto("/admin/operator-keys");
  await expect(page.getByRole("link", { name: "Sign in as an operator" })).toHaveAttribute("href", "/admin/login?next=%2Fadmin%2Foperator-keys");
  await page.getByRole("link", { name: "Sign in as an operator" }).click();
  await expect(page.getByRole("heading", { name: "Operator sign-in", exact: true })).toBeVisible();
});
