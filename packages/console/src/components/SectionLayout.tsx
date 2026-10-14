import { useLocation } from "@solidjs/router";
import { Show, type JSX } from "solid-js";
import { useScopeIDs } from "../lib/scope";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState } from "../ui/QueryState";
import { selectedLinkTab } from "../ui/linkTab";
import { LinkTabs, TabPanel, type LinkTabItem } from "../ui/Tabs";

// A header section whose pages are sub-tabs (Agents | Sessions | Triggers,
// Computers | Templates): one page header and tablist that stay mounted while the
// selected page renders in the tab panel. Every page needs the tab's Project and
// Environment.
export function SectionLayout<T extends string>(props: {
  id: string;
  title: string;
  tabs: readonly LinkTabItem<T>[];
  children?: JSX.Element;
}) {
  const location = useLocation();
  const scope = useScopeIDs();
  const selected = () => selectedLinkTab(props.tabs, location.pathname);
  return (
    <Page>
      <PageHeader title={props.title} />
      <LinkTabs id={props.id} label={props.title} items={props.tabs} selected={selected()} />
      <TabPanel tabs={props.id} value={selected()} selected={selected()}>
        <Show when={scope().projectID && scope().environmentID} fallback={<EmptyState title="Select a project and environment." />}>
          {props.children}
        </Show>
      </TabPanel>
    </Page>
  );
}
