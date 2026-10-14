import { useScopeIDs } from "../lib/scope";
import { scopeShareURL } from "../lib/scope-link";
import { ui } from "../ui/styles";
import { useClipboard, type CopyState } from "../ui/useClipboard";

const LABEL: Record<CopyState, string> = { idle: "Copy link", copied: "Copied", failed: "Could not copy" };

// Copies a link to the current page that opens it in this tab's Project and Environment.
export function CopyLinkButton() {
  const scope = useScopeIDs();
  const clipboard = useClipboard({ copied: "Link copied", failed: "Could not copy link" });

  return (
    <>
      <button
        type="button"
        class={ui.secondaryButton}
        disabled={!scope().projectID || !scope().environmentID}
        onClick={() => void clipboard.copy(scopeShareURL(window.location.href, scope()))}
      >
        {LABEL[clipboard.state()]}
      </button>
      <clipboard.LiveRegion />
    </>
  );
}
