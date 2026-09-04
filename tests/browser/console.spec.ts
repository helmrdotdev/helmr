import { expect, test } from "@playwright/test";

test("local developer can enter the console", async ({ page }) => {
  await page.goto("/dev/login");

  await expect(page).toHaveURL("/");
  await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();
  const navigation = page.getByRole("navigation", { name: "Sections" });
  for (const name of ["Dashboard", "Tasks", "Runs", "Schedules", "Settings"]) {
    await expect(navigation.getByRole("link", { name, exact: true })).toBeVisible();
  }
});
