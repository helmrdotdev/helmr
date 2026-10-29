import { authenticationResponse } from "./fixtures/authentication";
import { expect, test, type Page, type BrowserContext } from "@playwright/test";

// Real console routing/components; external authentication and API responses are
// fixtures. Server authority and cookie contracts have Go/PostgreSQL coverage.
async function fixture(context: BrowserContext, options: { loggedIn?: boolean; newUser?: boolean; status?: string; meError?: number } = {}) {
  const state = {
    loggedIn: options.loggedIn ?? false,
    userID:"user-1", name:"Developer", orgID:"org-1", orgName:"Example org",
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
    const account = {user_id:state.userID,display_name:state.name,org_id:state.orgRequired?null:state.orgID,org_name:state.orgName,permissions:[],organization_required:state.orgRequired,project_required:state.projectRequired,app_origin:url.origin};
    if(request.method()==="GET" && ["/api/auth/config","/api/organizations","/api/organization"].includes(url.pathname))return json(authenticationResponse(url.pathname,account));
    if(/^\/api\/auth\/email\/[^/]+\/verify$/.test(url.pathname)) {
      state.loggedIn=true;
      return json({redirect_after:state.next.get(url.pathname.split("/")[4]!)??"/"});
    }
    switch (url.pathname) {
      case "/api/me":
        if (state.meError) return json({ error: { code: "internal_error", message: "unavailable" } }, state.meError);
        if (!state.loggedIn) return json({ error: { code: "authentication_required", message: "Sign in" } }, 403);
        return json(authenticationResponse("/api/me",account));
      case "/api/auth/github/start": {
        const id = String(state.next.size + 1); state.next.set(id, body.next);
        return json({ redirect_url: `${url.origin}/auth/github/callback?code=fixture&state=${id}` });
      }
      case "/api/auth/email/start": {
        const id = String(state.next.size + 1); state.next.set(id, body.next);
        return json({challenge_id:id,expires_at:new Date(Date.now()+600000).toISOString(),resend_after:new Date(Date.now()+60000).toISOString()},202);
      }
      case "/api/auth/github/finish":
        state.loggedIn = true;
        return json({ redirect_after: state.next.get(body.state ?? body.token) ?? "/" });
      case "/api/organizations":
        state.orgRequired = false;
        return json({ id: "org-1", name: body.name, slug: body.slug }, 201);
      case "/api/auth/device/status":
        return json({ status: state.status, confirmation:false, consent:{account_name:state.name,org_name:state.orgName,org_id:state.orgID,proof:"fixture-consent"}, expires_at: new Date(Date.now() + 600_000).toISOString() });
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

// The value of one entry in the device request's details.
const detail = (page: Page, label: string) => page.getByLabel("Request").locator("dt", { hasText: label }).locator("xpath=following-sibling::dd[1]");

async function signIn(page: Page, method: "github" | "email") {
  if (method === "github") await page.getByRole("button", { name: "Continue with GitHub" }).click();
  else {
    await page.getByLabel("Email").fill("developer@example.test");
    await page.getByRole("button", { name: "Send verification code" }).click();
    await page.getByLabel("Verification code").fill("12345678");
    await page.getByRole("button", { name: "Verify email", exact:true }).click();
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
    await expect(detail(page, "Organization")).toHaveText("Example org");
    expect(state.projectCreates).toBe(0);
    expect(state.approved).toEqual([]);
    await page.getByRole("button", { name: "Approve", exact: true }).click();
    await expect(page.getByRole("heading", { name: "CLI authorized" })).toBeVisible();
    await expect(page.getByText("Return to your terminal to finish signing in.")).toBeVisible();
    expect(state.approved).toEqual([{ user_code: "ABCD-EFGH", org_id: "org-1" }]);
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
  await page.goto("/sessions?status=open#history");
  await signIn(page, "github");
  await expect(page.getByRole("heading", { name: "Create your first project" })).toBeVisible();
  await page.getByLabel("Name", { exact: true }).fill("Example project");
  await expect(page.getByText("Session history retention", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Create project", exact: true }).click();
  await expect(page).toHaveURL("/sessions?status=open#history");
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
    const titles: Record<string, string> = { expired: "This request expired", denied: "CLI sign-in denied", consumed: "This request was already used" };
    await expect(page.getByRole("heading", { name: titles[status] })).toBeVisible();
    await expect(page.getByText(status === "consumed" ? "Check your terminal for the login result." : "Run helmr login again in your terminal.")).toBeVisible();
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
  const state = await fixture(context, { loggedIn: true });
  await page.goto("/auth/device?code=CHANGED");
  await expect(detail(page, "Account")).toHaveText("Developer");
  state.userID="user-2";state.name="Another user";state.orgID="org-2";state.orgName="Another org";
  let attempts = 0;
  await context.route("**/api/auth/device/approve", async (route) => {
    attempts++;
    if (attempts === 1) return route.fulfill({ status: 409, json: { error: { code: "device_identity_changed", message: "Account changed. Review again." } } });
    expect(route.request().postDataJSON()).toEqual({ user_code: "CHANGED", org_id: "org-2" });
    return route.fulfill({ json: { status: "approved" } });
  });
  await page.getByRole("button", { name: "Approve", exact: true }).click();
  await expect(detail(page, "Account")).toHaveText("Another user");
  await expect(page.getByRole("alert")).toHaveText("Your account changed since this page loaded. Review the account and organization, then approve again.");
  await expect(page.locator("details").filter({ hasText: "Technical details" })).toContainText("device_identity_changed: Account changed. Review again.");
  expect(attempts).toBe(1);
  await page.getByRole("button", { name: "Approve", exact: true }).click();
  await expect(page.getByRole("heading", { name: "CLI authorized" })).toBeVisible();
  await expect(page.getByText("Return to your terminal to finish signing in.")).toBeVisible();
  expect(attempts).toBe(2);
});

for (const method of ["github"]) {
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
  await expect(page.getByText("Session history retention", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Create project", exact: true }).click();
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
  await expect(page.getByRole("heading", { name: "This request was already used" })).toBeVisible();
  await expect(page.getByText("Old action failed")).toHaveCount(0);
});

test("organization choice preserves the CLI consent destination",async({page,context})=>{
 await fixture(context,{loggedIn:true});
 await page.goto("/organizations?next=%2Fauth%2Fdevice%3Fuser_code%3DABCD-EFGH");
 await page.getByRole("button",{name:"Example org",exact:true}).click();
 await expect(page).toHaveURL(/\/auth\/device\?user_code=ABCD-EFGH/);
});

for(const membership of ["none","denied"]){
 test(`account confirmation remains available with ${membership} organization access`,async({page,context})=>{
 await fixture(context,{loggedIn:true});
 await context.route("**/api/organization",route=>route.fulfill({status:403,json:{error:{code:"access_denied",message:"Membership access ended"}}}));
 if(membership==="none")await context.route("**/api/organizations",route=>route.fulfill({json:{organizations:[]}}));
 await context.route("**/api/account/credentials",route=>route.fulfill({json:{credentials:[]}}));
 await page.goto("/settings/account");
 await expect(page.getByRole("heading",{name:"My account",exact:true})).toBeVisible();
 await expect(page.getByRole("heading",{name:"Confirm your account",exact:true})).toBeVisible();
 await expect(page).toHaveURL(/\/settings\/account$/);
 if(membership==="denied"){
 await page.goto("/");await expect(page.getByRole("link",{name:"Confirm sign-in",exact:true})).toBeVisible();
 await page.getByRole("link",{name:"Confirm sign-in",exact:true}).click();await expect(page.getByRole("heading",{name:"Confirm your account",exact:true})).toBeVisible();
 }
 });
}
