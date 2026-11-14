// Confirmation for destructive actions (Reject, Cancel, Forget, Remove repo)
// and for Approve. The caller awaits ctx.confirm.ask(...).
import { button } from "../ui.js";
import { Modal } from "./Modal.js";

const { h } = window.Vue;

export const ConfirmDialog = {
  props: { confirm: { type: Object, required: true } },
  setup(props) {
    return () => {
      const c = props.confirm, s = c.state;
      if (!s.open) return null;
      return h(Modal, { label: s.title, onClose: () => c.answer(false) }, () => [
        h("h2", s.title),
        s.message ? h("p", s.message) : null,
        s.details.length ? h("dl", { class: "facts" }, s.details.flatMap(([k, v]) => [h("dt", k), h("dd", { class: "mono" }, v)])) : null,
        h("div", { class: "actions" }, [
          button(s.confirmLabel, () => c.answer(true), { kind: s.danger ? "danger" : "primary" }),
          h("button", { type: "button", class: "btn", "data-autofocus": "", onClick: () => c.answer(false) }, "Keep it as is")
        ])
      ]);
    };
  }
};
