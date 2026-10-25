import { useScopeIDs } from "../lib/scope";
import { scopeShareURL } from "../lib/scope-link";
import type { ActionMenuItem } from "../ui/ActionMenu";
import { useClipboard } from "../ui/useClipboard";

// "Copy link" for an object menu: a link that opens the page in this tab's
// Project and Environment. Without `path` it is the current page. Render
// `LiveRegion` once where the menu is, so the result is announced.
export function useCopyLink() {
  const scope = useScopeIDs();
  const clipboard = useClipboard({ copied: "Link copied", failed: "Could not copy link" });
  const item = (path?: string): ActionMenuItem => ({
    label: "Copy link",
    disabled: !scope().projectID || !scope().environmentID,
    onSelect: () => void clipboard.copy(scopeShareURL(path ? new URL(path, window.location.origin).href : window.location.href, scope())),
  });
  return { item, LiveRegion: clipboard.LiveRegion };
}
