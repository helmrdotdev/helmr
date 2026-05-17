import { ui } from "./styles";

export function BrandMark() {
  return (
    <span class={ui.brandMark} aria-hidden="true">
      <svg class="size-2.5" viewBox="0 0 14 14" fill="none">
        <path d="M7 1L12.196 4V10L7 13L1.804 10V4L7 1Z" fill="currentColor" />
      </svg>
    </span>
  );
}
