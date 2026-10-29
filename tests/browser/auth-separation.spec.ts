import { expect, test } from "@playwright/test";
import { authenticationResponse, isAuthenticationPath } from "./fixtures/authentication";

// Real Console components and routing; deterministic HTTP responses isolate
// interruption behavior from server authorization, covered by PostgreSQL tests.
for (const interruption of ["confirmation", "expired", "background-expiry"] as const) {
  test(`secret form survives ${interruption} and only saves on explicit retry`, async ({ page, context }) => {
    let authenticated = true;
    let confirmed = false;
    let attempts = 0;
    const now = new Date().toISOString();
    const environment = { environment: "production", project: "project", name: "Production", color_hex: "#1f7a4d", is_default: true, created_at: now, updated_at: now };
    const project = { project: "project", name: "Project", is_default: true, environments: [environment], created_at: now, updated_at: now };
    await context.route("**/api/**", async route => {
      const request = route.request();
      const path = new URL(request.url()).pathname;
      if (path === "/api/me" && !authenticated) return route.fulfill({ status: 403, json: { error: { code: "authentication_required", message: "Sign in" } } });
      if (isAuthenticationPath(path)) return route.fulfill({ json: authenticationResponse(path, { user_id: "original-user", display_name: "Original", org_id: "org", permissions: ["secrets.read", "secrets.create"], project_required: false }) });
      if (path === "/api/projects") return route.fulfill({ json: { projects: [project] } });
      if (path === "/api/projects/project") return route.fulfill({ json: project });
      if (path.endsWith("/secrets")) {
        if (request.method() === "GET") return route.fulfill({ json: { secrets: [] } });
        attempts++;
        expect(request.headers()["x-helmr-organization"]).toBe("org");
        expect(request.headers()["x-helmr-actor"]).toBe("original-user");
        if (!confirmed) {
          if (interruption === "expired") authenticated = false;
          return route.fulfill({ status: 403, json: { error: { code: interruption === "expired" ? "authentication_required" : "recent_authentication_required", message: "Confirm sign-in" } } });
        }
        expect(request.postDataJSON()).toMatchObject({ secret: "retained-secret", value: "form-only-private-value" });
        return route.fulfill({ status: 201, json: { secret: "retained-secret", status: "active", created_at: now } });
      }
      return route.fulfill({ status: 404, json: { error: { code: "not_found", message: path } } });
    });
    if(interruption==="background-expiry")await page.clock.install({time:new Date()});
    await page.goto("/settings/secrets?project=project&environment=production");
    await page.getByRole("button", { name: "Set secret", exact: true }).click();
    const form = page.getByRole("dialog", { name: "Set secret", exact: true });
    await form.getByLabel("Secret ID", { exact: true }).fill("retained-secret");
    await form.getByLabel("Value", { exact: true }).fill("form-only-private-value");
    if(interruption==="background-expiry"){
      authenticated=false;
      await page.clock.fastForward(61000);
      await page.evaluate(()=>window.dispatchEvent(new Event("visibilitychange")));
    }else await form.getByRole("button", { name: "Save", exact: true }).click();
    const notice = page.getByRole("dialog", { name: "Confirm your sign-in", exact: true });
    await expect(notice).toBeVisible();
    const link = notice.getByRole("link", { name: "Confirm sign-in", exact: true });
    await expect(link).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(notice.getByRole("button", { name: "Return to form", exact: true })).toBeFocused();
    const [confirmation] = await Promise.all([context.waitForEvent("page"), link.click()]);
    await confirmation.waitForLoadState();
    authenticated = true; confirmed = true;
    await confirmation.close();
    await page.bringToFront();
    await notice.getByRole("button", { name: "Return to form", exact: true }).click();
    await expect(form.getByLabel("Secret ID", { exact: true })).toHaveValue("retained-secret");
    await expect(form.getByLabel("Value", { exact: true })).toHaveValue("form-only-private-value");
    expect(attempts).toBe(interruption==="background-expiry"?0:1);
    const stored = await page.evaluate(() => JSON.stringify({ session: { ...sessionStorage }, local: { ...localStorage } }));
    expect(stored).not.toContain("form-only-private-value");
    await form.getByRole("button", { name: "Save", exact: true }).click();
    await expect(form).toHaveCount(0);
    expect(attempts).toBe(interruption==="background-expiry"?1:2);
  });
}
