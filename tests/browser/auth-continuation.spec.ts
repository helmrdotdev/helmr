import { authenticationResponse } from "./fixtures/authentication";
import { expect, test, type Page, type BrowserContext } from "@playwright/test";

// Real console routing/components; external authentication and API responses are
// fixtures. Server authority and cookie contracts have Go/PostgreSQL coverage.
async function fixture(context: BrowserContext, options: { loggedIn?: boolean; newUser?: boolean; status?: string; meError?: number } = {}) {
  const state = {
    loggedIn: options.loggedIn ?? false,
    userID:"user-1", primaryEmail:"fixture@example.test", name:"Developer", orgID:"org-1", orgName:"Example org",
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
    const account = {primary_email:state.primaryEmail,user_id:state.userID,display_name:state.name,org_id:state.orgRequired?null:state.orgID,org_name:state.orgName,permissions:[],organization_required:state.orgRequired,project_required:state.projectRequired,app_origin:url.origin};
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

test("organization page marks the current organization and returns to it",async({page,context})=>{
 await fixture(context,{loggedIn:true});
 await page.goto("/");
 await page.goto("/organizations");
 await expect(page.getByRole("heading",{name:"Choose an organization",exact:true})).toBeVisible();
 await expect(page.getByRole("button",{name:"Example org",exact:true})).toHaveAttribute("aria-current","true");
 await expect(page.getByRole("link",{name:"Back to Example org",exact:true})).toHaveAttribute("href","/");
});

test("an invitation link that no longer works says what to do next",async({page,context})=>{
 await fixture(context,{loggedIn:false});
 await context.route("**/api/auth/invitations/resolve",route=>route.fulfill({status:400,json:{error:{code:"invalid_token",message:"token is invalid or expired"}}}));
 await page.goto("/invite?token=expired-invitation");
 await expect(page.getByRole("heading",{name:"This invitation link does not work",exact:true})).toBeVisible();
 await expect(page.getByRole("alert")).toHaveText("The link is invalid or has expired.");
 await expect(page.getByText("token is invalid or expired")).toHaveCount(0);
 await expect(page.getByRole("link",{name:"Sign in to Helmr",exact:true})).toHaveAttribute("href","/login");
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
 await page.goto("/settings/security");
 await expect(page.getByRole("heading",{name:"Sign-in and security",exact:true})).toBeVisible();
 await expect(page).toHaveURL(/\/settings\/security$/);
 if(membership==="denied"){
 await page.goto("/");await page.getByRole("button",{name:"Confirm sign-in",exact:true}).click();
 const confirmation=page.getByRole("dialog",{name:"Confirm your sign-in",exact:true});
 await expect(confirmation.getByRole("button",{name:"Send verification code",exact:true})).toBeVisible();
 }
 });
}

test("creating another organization selects it before project onboarding",async({page,context})=>{
 const state=await fixture(context,{loggedIn:true});
 await page.goto("/auth/device?code=EXISTING");
 await expect(detail(page,"Organization")).toHaveText("Example org");
 expect(await page.evaluate(()=>sessionStorage.getItem("helmr.organization"))).toBe("org-1");
 await context.route("**/api/organizations",async route=>{
  if(route.request().method()!=="POST")return route.fallback();
  state.orgID="org-2";state.orgName="New organization";state.projectRequired=true;
  return route.fulfill({status:201,json:{id:"org-2",name:"New organization",slug:"new-organization"}});
 });
 await page.goto("/organizations/new");
 await page.getByLabel("Name",{exact:true}).fill("New organization");
 await page.getByRole("button",{name:"Create organization",exact:true}).click();
 await expect(page).toHaveURL(/\/projects\/new$/);
 expect(await page.evaluate(()=>sessionStorage.getItem("helmr.organization"))).toBe("org-2");
});

test("email confirmation follows a changed login address without reload",async({page,context})=>{
 const state=await fixture(context,{loggedIn:true});
 await context.route("**/api/account/credentials",route=>route.fulfill({json:{credentials:[]}}));
 await context.route("**/api/account/email-change/start",route=>route.fulfill({json:{challenge_id:"change",expires_at:new Date(Date.now()+600000).toISOString(),resend_after:new Date(Date.now()+60000).toISOString()}}));
 await context.route("**/api/account/email-change/verify",route=>{state.primaryEmail="changed@example.test";return route.fulfill({status:204});});
 await page.goto("/settings/account");
 await page.getByRole("button",{name:"Change…",exact:true}).click();
 const change=page.getByRole("dialog",{name:"Change login email",exact:true});
 await change.getByLabel("New email",{exact:true}).fill("changed@example.test");
 await change.getByRole("button",{name:"Send code",exact:true}).click();
 await change.getByLabel("Verification code",{exact:true}).fill("12345678");
 await change.getByRole("button",{name:"Change email",exact:true}).click();
 await expect(change).toHaveCount(0);
 await expect(page.getByRole("region",{name:"Your profile",exact:true})).toContainText("changed@example.test");
 await page.evaluate(()=>window.dispatchEvent(new CustomEvent("helmr-confirmation-required",{detail:{operator:false}})));
 const confirmation=page.getByRole("dialog",{name:"Confirm your sign-in",exact:true});
 await expect(confirmation).toContainText("changed@example.test");
 const sent=page.waitForRequest(request=>new URL(request.url()).pathname==="/api/auth/email/start");
 await confirmation.getByRole("button",{name:"Send verification code",exact:true}).click();
 expect((await sent).postDataJSON().email).toBe("changed@example.test");
});

test("invitation survives switching out of a different account",async({page,context})=>{
 const state=await fixture(context,{loggedIn:true});
 await context.route("**/api/auth/invitations/resolve",route=>route.fulfill({json:{organization_id:"invited-org",organization_name:"Invited organization",email:"invited@example.test",expires_at:new Date(Date.now()+600000).toISOString()}}));
 await context.route("**/api/auth/logout",route=>{state.loggedIn=false;return route.fulfill({status:204});});
 await page.goto("/invite?token=private-invitation");
 await expect(page).toHaveURL(/\/invite$/);
 await expect(page.getByRole("status")).toContainText("Sign in with the invited email to accept.");
 await page.getByRole("button",{name:"Sign out to use another account",exact:true}).click();
 await expect(page.getByLabel("Email",{exact:true})).toHaveValue("invited@example.test");
 const sent=page.waitForRequest(request=>new URL(request.url()).pathname==="/api/auth/email/start");
 await page.getByRole("button",{name:"Send verification code",exact:true}).click();
 expect((await sent).postDataJSON().invitation_token).toBe("private-invitation");
 await context.route("**/api/auth/email/*/verify",route=>{state.loggedIn=true;state.userID="invited-user";state.primaryEmail="invited@example.test";return route.fulfill({json:{redirect_after:"/"}});});
 await page.getByLabel("Verification code",{exact:true}).fill("12345678");
 await page.getByRole("button",{name:"Verify email",exact:true}).click();
 await expect(page.getByRole("button",{name:"Accept invitation",exact:true})).toBeEnabled();
 await expect(page).toHaveURL(/\/invite$/);
});

for (const method of ["email", "github"] as const) {
 test(`CLI approval confirms with ${method} in context without automatic approval`, async ({page,context}) => {
  const state=await fixture(context,{loggedIn:true});
  let attempts=0;
  await context.route("**/api/auth/device/approve",route=>{
   attempts++;
   if(attempts===1)return route.fulfill({status:403,json:{error:{code:"recent_authentication_required",message:"Confirm sign-in"}}});
   return route.fallback();
  });
  await page.goto("/auth/device?code=RETAINED");
  await page.getByRole("button",{name:"Approve",exact:true}).click();
  const dialog=page.getByRole("dialog",{name:"Confirm your sign-in"});
  await expect(dialog).toBeVisible();
  if(method==="email"){
   await dialog.getByRole("button",{name:"Send verification code"}).click();
   await dialog.getByLabel("Verification code").fill("12345678");
   await dialog.getByRole("button",{name:"Verify email",exact:true}).click();
  }else{
   await dialog.getByRole("button",{name:"Confirm with GitHub"}).click();
  }
  await expect(dialog.getByRole("status")).toContainText("Sign-in confirmed");
  expect(attempts).toBe(1);expect(state.approved).toEqual([]);
  await dialog.getByRole("button",{name:"Return to your action"}).click();
  await expect(page.getByLabel("Device code")).toHaveText("RETAINED");
  await page.getByRole("button",{name:"Approve",exact:true}).click();
  await expect(page.getByRole("heading",{name:"CLI authorized"})).toBeVisible();
  expect(attempts).toBe(2);expect(state.approved).toHaveLength(1);
 });
}

test("credential removal keeps its confirmation open through sign-in confirmation and cancellation", async ({page,context}, testInfo) => {
 await fixture(context,{loggedIn:true});
 let removals=0;
 await context.route("**/api/account/credentials",route=>route.fulfill({json:{credentials:[{id:"key-1",method:"passkey",label:"Passkey 1",created_at:new Date().toISOString()}]}}));
 await context.route("**/api/account/credentials/key-1",route=>{
  removals++;
  if(removals<3)return route.fulfill({status:403,json:{error:{code:"recent_authentication_required",message:"Use a retained method"}}});
  return route.fulfill({status:204});
 });
 await page.goto("/settings/security");
 await page.getByRole("button",{name:"Actions for Passkey 1",exact:true}).click();
 await page.getByRole("menuitem",{name:"Remove",exact:true}).click();
 const remove=page.getByRole("dialog",{name:"Remove sign-in method",exact:true});
 await remove.getByRole("button",{name:"Remove method",exact:true}).click();
 const dialog=page.getByRole("dialog",{name:"Confirm your sign-in"});
 await expect(dialog).toContainText("Needed for");
 await expect(dialog).toContainText("Use a retained method");
 await page.screenshot({path:testInfo.outputPath("account-confirmation.png"),fullPage:true});
 await dialog.getByRole("button",{name:"Cancel",exact:true}).click();
 await expect(remove).toContainText("Passkey 1");expect(removals).toBe(1);
 await remove.getByRole("button",{name:"Remove method",exact:true}).click();
 await dialog.getByRole("button",{name:"Send verification code"}).click();
 await dialog.getByLabel("Verification code").fill("12345678");
 await dialog.getByRole("button",{name:"Verify email",exact:true}).click();
 await dialog.getByRole("button",{name:"Return to your action"}).click();
 await expect(remove).toContainText("Passkey 1");expect(removals).toBe(2);
 await remove.getByRole("button",{name:"Remove method",exact:true}).click();
 await expect(page.getByText("Sign-in method removed.", {exact:true})).toBeVisible();
 expect(removals).toBe(3);
});
