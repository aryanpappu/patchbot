// Keyboard shortcut reference, opened with ?.
import { Modal } from "./Modal.js";

const { h } = window.Vue;
const rows = [
  ["g i", "Go to the inbox"], ["g r", "Go to runs"], ["g n", "Go to new request"], ["a", "Approve (opens a confirmation)"],
  ["r", "Revise (focus the revision notes)"], ["?", "Show this help"], ["Esc", "Close a dialog"]
];

export const HelpDialog = {
  props: { open: Boolean },
  emits: ["close"],
  setup(props, { emit }) {
    return () => props.open ? h(Modal, { label: "Keyboard shortcuts", onClose: () => emit("close") }, () => [
      h("h2", "Keyboard shortcuts"),
      h("p", { class: "muted" }, "Shortcuts are ignored while you type in a field. a and r only work on a run that is waiting for you."),
      h("dl", { class: "facts" }, rows.flatMap(([k, v]) => [h("dt", h("kbd", k)), h("dd", v)])),
      h("div", { class: "actions" }, [h("button", { type: "button", class: "btn primary", "data-autofocus": "", onClick: () => emit("close") }, "Close")])
    ]) : null;
  }
};
