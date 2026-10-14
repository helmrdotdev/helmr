import { createSignal, For } from "solid-js";
import { ui } from "../../ui/styles";

const colors = ["violet", "teal", "orange", "blue", "pink", "yellow"] as const;
const icons = [...colors, "light", "dark"] as const;
type Icon = (typeof icons)[number];

// Slack manifests cannot carry an app icon, so each Agent starts from a stable
// color and the admin uploads the downloaded image in Slack.
function defaultIcon(seed: string): Icon {
  let hash = 0;
  for (const char of seed) hash = (hash * 31 + char.charCodeAt(0)) >>> 0;
  return colors[hash % colors.length]!;
}

export function AppIconPicker(props: { seed: string; agentName: string }) {
  const [icon, setIcon] = createSignal(defaultIcon(props.seed));
  return <fieldset class="flex flex-col gap-2">
    <legend class="mb-2">App icon</legend>
    <p class={ui.muted}>Download an icon, then upload it in Slack on the app’s Basic Information page under Display Information.</p>
    <div class="flex flex-wrap gap-2">
      <For each={icons}>{(name) => <label class="cursor-pointer">
        <input class="peer sr-only" type="radio" name="slack-app-icon" value={name} checked={icon() === name} onChange={() => setIcon(name)} aria-label={`${name[0]!.toUpperCase()}${name.slice(1)} icon`} />
        <img class="size-10 rounded-md border border-console-border peer-checked:border-2 peer-checked:border-console-accent peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-solid" src={`/slack-app-icons/${name}.png`} alt="" width="40" height="40" />
      </label>}</For>
    </div>
    <div><a class={ui.secondaryButton} href={`/slack-app-icons/${icon()}.png`} download={`${props.agentName}-slack-icon.png`}>Download icon</a></div>
  </fieldset>;
}
