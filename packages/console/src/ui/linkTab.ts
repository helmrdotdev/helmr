// The tab whose page is at `pathname`, ignoring a trailing slash, or the first tab.
export function selectedLinkTab<T extends string>(items: readonly { value: T; href: string }[], pathname: string): T {
  const path = pathname.replace(/\/+$/, "") || "/";
  return (items.find((item) => item.href === path) ?? items[0]!).value;
}
