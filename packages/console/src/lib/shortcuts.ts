// The modifier shortcut that opens the command palette: Cmd+K on Apple
// platforms and Ctrl+K elsewhere, never the other platform's combination (so
// macOS Ctrl+K keeps deleting to the end of the line in text fields). Without
// the modifier nothing fires, so typing in inputs never opens it, and nothing
// fires while an input method is composing.
export function isPaletteShortcut(
  event: Pick<KeyboardEvent, "key" | "metaKey" | "ctrlKey" | "altKey" | "shiftKey" | "isComposing" | "keyCode">,
  apple: boolean,
): boolean {
  if (event.isComposing || event.keyCode === 229) return false;
  if (event.key.toLowerCase() !== "k" || event.altKey || event.shiftKey) return false;
  return apple ? event.metaKey && !event.ctrlKey : event.ctrlKey && !event.metaKey;
}

export function isApplePlatform(): boolean {
  return typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
}
