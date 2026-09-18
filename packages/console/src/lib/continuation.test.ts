import { expect, test } from "bun:test";
import { locationDestination, returnPath, withNext } from "./continuation";
import { onboardingRedirectPath, type Me } from "./auth";

test("preserves the requested page, query and fragment across login and onboarding", () => {
  const destination = "/auth/device?code=ABCD-EFGH#confirm";
  for (const path of ["/login", "/organizations/new", "/projects/new", "/access-required"]) {
    const url = new URL(withNext(path, destination), "https://helmr.test");
    expect(locationDestination(url)).toBe(destination);
  }
  expect(locationDestination(new URL("https://helmr.test/runs/123?view=logs#last")))
    .toBe("/runs/123?view=logs#last");
});

test("rejects external, malformed, and looping destinations", () => {
  for (const path of [undefined, "", "https://evil.test", "//evil.test", "/\\evil.test", "/%2f/evil.test", "/%5cevil.test", "/bad\npath", "/%zz", "/login", "/%6cogin", "/runs/../login", "/auth/github/callback?code=secret", "/auth/magic-link/callback", "/organizations/new?next=/", "/projects/new?next=/projects/new", "/access-required", "/" + "x".repeat(256)]) {
    expect(returnPath(path)).toBe("/");
  }
  expect(returnPath("/runs/../sessions?state=waiting")).toBe("/sessions?state=waiting");
});

test("independent requests do not overwrite one another", () => {
  const a = withNext("/login", "/auth/device?code=AAAA");
  const b = withNext("/login", "/auth/device?code=BBBB");
  expect(locationDestination(new URL(a, "https://helmr.test"))).toBe("/auth/device?code=AAAA");
  expect(locationDestination(new URL(b, "https://helmr.test"))).toBe("/auth/device?code=BBBB");
});

const me: Me = { user_id: "user", display_name: null, profile_image_url: null, admin: false, permissions: [], organization_required: false, project_required: true };

test("enforces only the prerequisites of the requested operation", () => {
  expect(onboardingRedirectPath(me, "organization")).toBeNull();
  expect(onboardingRedirectPath(me, "project")).toBe("/projects/new");
  expect(onboardingRedirectPath({ ...me, organization_required: true }, "organization")).toBe("/organizations/new");
  expect(onboardingRedirectPath({ ...me, access_required: true }, "organization")).toBe("/access-required");
  expect(onboardingRedirectPath({ ...me, access_required: true }, "session")).toBeNull();
});

test("direct project creation survives login without becoming its own completion target", () => {
  expect(returnPath("/projects/new")).toBe("/projects/new");
  expect(locationDestination(new URL("https://helmr.test/projects/new"))).toBe("/projects/new");
});
