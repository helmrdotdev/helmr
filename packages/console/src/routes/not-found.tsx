import { A } from "@solidjs/router";
import { AuthActions, AuthCopy, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { PageHeader } from "../ui/PageHeader";
import { ui } from "../ui/styles";

const COPY = "There is no page at this address. It may have moved, or the link may be mistyped.";

// A path no route matches, inside the console for a signed-in account.
export function NotFound() {
  return (
    <PageHeader
      title="Page not found"
      subtitle={COPY}
      actions={<>
        <A class={ui.secondaryButton} href="/settings/project">Settings</A>
        <A class={ui.button} href="/">Overview</A>
      </>}
    />
  );
}

// The same, for a visitor without a console account to show it in.
export function NotFoundWindow() {
  return (
    <AuthWindow>
      <AuthTitle>Page not found</AuthTitle>
      <AuthCopy>{COPY}</AuthCopy>
      <AuthActions>
        <A class={ui.button} href="/">Go to the console</A>
      </AuthActions>
    </AuthWindow>
  );
}
