import { expect, test, type Page, type BrowserContext } from "@playwright/test";

// Real console routing/components; external authentication and API responses are
// fixtures. Server authority and cookie contracts have Go/PostgreSQL coverage.
async function fixture(context: BrowserContext, options: { loggedIn?: boolean; newUser?: boolean; status?: string; meError?: number } = {}) {
  const state = {
    loggedIn: options.loggedIn ?? false,
    orgRequired: options.newUser ?? false,
    projectRequired: true,
    status: options.status ?? "pending",
    meError: options.meError ?? 0,
    next: new Map<string, string>(),
    approved: [] as Record<string, string>[],
    projectCreates: 0,
  };
  await context.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const body = request.method() === "POST" ? request.postDataJSON() ?? {} : {};
    const json = (value: unknown, status = 200) => route.fulfill({ status, json: value });
    switch (url.pathname) {
      case "/api/me":
        if (state.meError) return json({ error: { code: "internal_error", message: "unavailable" } }, state.meError);
        if (!state.loggedIn) return json({ error: { code: "unauthorized", message: "Sign in" } }, 401);
        return json({ user_id: "user-1", display_name: "Developer", permissions: [], admin: false, org_id: state.orgRequired ? null : "org-1", org_name: "Example org", organization_required: state.orgRequired, project_required: state.projectRequired, public_url: url.origin });
      case "/api/auth/github/start": {
        const id = String(state.next.size + 1); state.next.set(id, body.next);
        return json({ redirect_url: `${url.origin}/auth/github/callback?code=fixture&state=${id}` });
      }
      case "/api/auth/magic-link/start": {
        const id = String(state.next.size + 1); state.next.set(id, body.next);
        return json({ sent: true, debug_url: `${url.origin}/auth/magic-link/callback?token=${id}` });
      }
      case "/api/auth/github/finish":
      case "/api/auth/magic-link/finish":
        state.loggedIn = true;
        return json({ redirect_after: state.next.get(body.state ?? body.token) ?? "/" });
      case "/api/organizations":
        state.orgRequired = false;
        return json({ id: "org-1", name: body.name, slug: body.slug }, 201);
      case "/api/auth/device/status":
        return json({ status: state.status, expires_at: new Date(Date.now() + 600_000).toISOString() });
      case "/api/auth/device/approve":
      case "/api/auth/device/deny":
        state.approved.push(body);
        state.status = url.pathname.endsWith("approve") ? "approved" : "denied";
        return json({ status: state.status });
      case "/api/regions":
        return json({ regions: [{ id: "test", display_name: "Test region" }] });
      case "/api/projects":
        if (request.method() === "POST") {
          state.projectCreates++; state.projectRequired = false;
          return json({ id: "project-1", org_id: "org-1", name: body.name, slug: body.slug, environments: [] }, 201);
        }
        return json({ projects: [] });
      default:
        return json({ error: { code: "not_found", message: `Unmocked ${url.pathname}` } }, 404);
    }
  });
  return state;
}

async function signIn(page: Page, method: "github" | "email") {
  if (method === "github") await page.getByRole("button", { name: "Continue with GitHub" }).click();
  else {
    await page.getByLabel("Email").fill("developer@example.test");
    await page.getByRole("button", { name: "Send sign-in link" }).click();
    await page.getByRole("button", { name: "Continue with dev link" }).click();
  }
}

for (const method of ["github", "email"] as const) {
  test(`${method}: first login creates only the required org and resumes CLI consent`, async ({ page, context }) => {
    const state = await fixture(context, { newUser: true });
    await page.goto("/auth/device?code=ABCD-EFGH");
    await expect(page).toHaveURL(/\/login\?next=/);
    await signIn(page, method);
    await expect(page.getByRole("heading", { name: "Create your organization" })).toBeVisible();
    await page.getByLabel("Name", { exact: true }).fill("Example org");
    await page.getByRole("button", { name: "Create organization" }).click();
    await expect(page).toHaveURL("/auth/device?code=ABCD-EFGH");
    await expect(page.getByText("Organization: Example org")).toBeVisible();
    expect(state.projectCreates).toBe(0);
    expect(state.approved).toEqual([]);
    await page.getByRole("button", { name: "Approve", exact: true }).click();
    await expect(page.getByText("Approved. Return to your terminal to finish signing in.")).toBeVisible();
    expect(state.approved).toEqual([{ user_code: "ABCD-EFGH", user_id: "user-1", org_id: "org-1" }]);
  });
}

test("existing org needs no project or additional login for CLI approval", async ({ page, context }) => {
  await fixture(context, { loggedIn: true });
  await page.goto("/auth/device?code=EXISTING");
  await expect(page.getByRole("button", { name: "Approve", exact: true })).toBeVisible();
  await expect(page).toHaveURL("/auth/device?code=EXISTING");
});

test("ordinary protected destinations return after required project creation", async ({ page, context }) => {
  const state = await fixture(context);
  await page.goto("/runs?kind=task#logs");
  await signIn(page, "github");
  await expect(page.getByRole("heading", { name: "Create your first project" })).toBeVisible();
  await page.getByLabel("Name", { exact: true }).fill("Example project");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await expect(page).toHaveURL("/runs?kind=task#logs");
  expect(state.projectCreates).toBe(1);
});

test("account server failures show retry instead of sending the user to login", async ({ page, context }) => {
  const state = await fixture(context, { loggedIn: true, meError: 500 });
  await page.goto("/auth/device?code=RETRY");
  await expect(page.getByRole("heading", { name: "Could not load your account" })).toBeVisible();
  await expect(page).toHaveURL("/auth/device?code=RETRY");
  state.meError = 0;
  await page.getByRole("button", { name: "Retry", exact: true }).click();
  await expect(page.getByRole("button", { name: "Approve", exact: true })).toBeVisible();
});

for (const status of ["expired", "denied", "consumed"]) {
  test(`${status} requests cannot be approved again`, async ({ page, context }) => {
    await fixture(context, { loggedIn: true, status });
    await page.goto("/auth/device?code=OLD");
    await expect(page.getByRole("heading", { name: "Authorize CLI" })).toBeVisible();
    await expect(page.getByText(status === "consumed" ? "This request has already been used. Check your terminal for the login result." : "Run helmr login again in your terminal to start a new request.")).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve", exact: true })).toHaveCount(0);
  });
}

test("a crafted external next never leaves the console", async ({ page, context }) => {
  const state = await fixture(context);
  state.projectRequired = false;
  await page.goto("/login?next=https%3A%2F%2Fevil.example");
  await signIn(page, "github");
  await expect(page).toHaveURL("/");
  expect([...state.next.values()]).toEqual(["/"]);
});

test("two request URLs retain separate codes when login is already available", async ({ page, context }) => {
  await fixture(context, { loggedIn: true });
  const second = await context.newPage();
  await page.goto("/auth/device?code=FIRST");
  await second.goto("/auth/device?code=SECOND");
  await expect(page.getByLabel("Device code")).toHaveText("FIRST");
  await expect(second.getByLabel("Device code")).toHaveText("SECOND");
});

test("a changed account requires another explicit review and approval", async ({ page, context }) => {
  await fixture(context, { loggedIn: true });
  await page.goto("/auth/device?code=CHANGED");
  await expect(page.getByText("Account: Developer", { exact: true })).toBeVisible();
  await context.route("**/api/me", (route) => route.fulfill({ json: { user_id: "user-2", display_name: "Another user", org_id: "org-2", org_name: "Another org", permissions: [], organization_required: false, project_required: true, admin: false } }));
  let attempts = 0;
  await context.route("**/api/auth/device/approve", async (route) => {
    attempts++;
    if (attempts === 1) return route.fulfill({ status: 409, json: { error: { code: "device_identity_changed", message: "Account changed. Review again." } } });
    expect(route.request().postDataJSON()).toEqual({ user_code: "CHANGED", user_id: "user-2", org_id: "org-2" });
    return route.fulfill({ json: { status: "approved" } });
  });
  await page.getByRole("button", { name: "Approve", exact: true }).click();
  await expect(page.getByText("Account: Another user", { exact: true })).toBeVisible();
  await expect(page.getByText("Account changed. Review again.")).toBeVisible();
  expect(attempts).toBe(1);
  await page.getByRole("button", { name: "Approve", exact: true }).click();
  await expect(page.getByText("Approved. Return to your terminal to finish signing in.")).toBeVisible();
  expect(attempts).toBe(2);
});

for (const method of ["github", "magic-link"]) {
  test(`${method} callback failures distinguish CLI retry from console sign-in`, async ({ page, context }) => {
    await fixture(context);
    await context.route(`**/api/auth/${method}/finish`, route => route.fulfill({ status: 400, json: { error: { code: "invalid_request", message: "Sign-in failed" } } }));
    await page.goto(method === "github" ? "/auth/github/callback?code=old&state=old" : "/auth/magic-link/callback?token=old");
    await expect(page.getByText("If you started from the CLI, run helmr login again in your terminal.")).toBeVisible();
    await expect(page.getByRole("link", { name: "Sign in to the console" })).toHaveAttribute("href", "/login");
  });
}

test("direct project creation survives login and finishes at home", async ({ page, context }) => {
  const state = await fixture(context);
  state.projectRequired = false;
  await page.goto("/projects/new");
  await signIn(page, "github");
  await expect(page).toHaveURL("/projects/new");
  await page.getByLabel("Name", { exact: true }).fill("Another project");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await expect(page).toHaveURL("/");
  expect(state.projectCreates).toBe(1);
});

test("a request consumed elsewhere replaces the stale action error", async ({ page, context }) => {
  const state = await fixture(context, { loggedIn: true });
  await page.goto("/auth/device?code=STALE");
  await expect(page.getByRole("button", { name: "Approve", exact: true })).toBeVisible();
  state.status = "consumed";
  await context.route("**/api/auth/device/approve", route => route.fulfill({ status: 404, json: { error: { code: "not_found", message: "Old action failed" } } }));
  await page.getByRole("button", { name: "Approve", exact: true }).click();
  await expect(page.getByText("This request has already been used. Check your terminal for the login result.")).toBeVisible();
  await expect(page.getByText("Old action failed")).toHaveCount(0);
});
