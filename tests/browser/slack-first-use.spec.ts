import { expect, test } from "@playwright/test";

const destination = "/auth/slack/connect?link=fixture.signed";
const returnURL = "https://slack.com/app_redirect?channel=C1&team=T1";

test("first Slack use preserves its destination through Helmr login", async ({ page }) => {
  await page.route("**/api/slack/user-links/first-use?**", route => route.fulfill({ status: 401, json: { error: { code: "unauthorized", message: "Sign in" } } }));
  await page.goto(destination);
  const signIn = page.getByRole("link", { name: "Sign in to Helmr" });
  await expect(signIn).toHaveAttribute("href", `/login?next=${encodeURIComponent(destination)}`);
  await expect(page.getByText("Your action has not been applied.", { exact: false })).toBeVisible();
});

test("first Slack use explains missing membership without offering to join", async ({ page }) => {
  await page.route("**/api/slack/user-links/first-use?**", route => route.fulfill({ status: 403, json: { error: { code: "forbidden", message: "Access denied" } } }));
  await page.goto(destination);
  await expect(page.getByRole("alert")).toContainText("Ask an administrator for an invitation");
  await expect(page.getByRole("button", { name: "Use a different Helmr account" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Verify my Slack account" })).toHaveCount(0);
});

test("first-use confirmation returns to Slack only after explicit linking", async ({ page }) => {
  const confirmations: unknown[] = [];
  await page.route("**/api/slack/user-links/verify", route => route.fulfill({ json: {
    identity: { team_id: "T1", slack_user_id: "U1", name: "Slack Person" },
    confirmation: "confirmation", helmr_user_id: "user", helmr_display_name: "Helmr Person",
    helmr_org_name: "Organization", workspace_name: "Workspace", return_url: returnURL,
  } }));
  await page.route("**/api/slack/user-links/confirm", route => {
    confirmations.push(route.request().postDataJSON());
    return route.fulfill({ status: 204 });
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/auth/slack/link#state=state&code=code");
  await expect(page).toHaveURL("/auth/slack/link");
  await expect(page.getByText("Slack Person", { exact: true })).toBeVisible();
  await expect(page.getByText("Helmr Person", { exact: true })).toBeVisible();
  expect(confirmations).toEqual([]);
  await expect(page.getByRole("link", { name: "Return to Slack" })).toHaveCount(0);
  await page.getByRole("button", { name: "Confirm these accounts are mine" }).click();
  await expect(page.getByRole("heading", { name: "Your accounts are linked" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Return to Slack" })).toHaveAttribute("href", returnURL);
  await expect(page.getByText("Linking does not apply your earlier action.", { exact: false })).toBeVisible();
  expect(confirmations).toEqual([{ confirmation: "confirmation", confirmed: true }]);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});
