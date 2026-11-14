// Toast stack: role="status" so screen readers announce success feedback.
import { icon } from "../ui.js";

const { h } = window.Vue;

export const Toast = {
  props: { toast: { type: Object, required: true } },
  setup(props) {
    return () => h("div", { class: "toasts", role: "status", "aria-live": "polite" }, props.toast.state.items.map(item =>
      h("div", { class: "toast " + item.kind, key: item.id }, [
        icon(item.kind === "ok" ? "check-circle" : "warn", 16), h("span", item.message),
        h("button", { type: "button", class: "btn quiet", "aria-label": "Dismiss message", onClick: () => props.toast.dismiss(item.id) }, "×")
      ])));
  }
};
