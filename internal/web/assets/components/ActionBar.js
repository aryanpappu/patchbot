// Sticky Approve / Revise / Reject bar plus Cancel and Retry. Buttons show by
// the API's capabilities; the server still decides whether an action is valid.
import { shortHash } from "../format.js";
import { area, button, field } from "../ui.js";
import { modelFields } from "./ModelFields.js";

const { h } = window.Vue;

export const ActionBar = {
  props: { d: Object, s: Object, busy: Boolean, providers: Object, onApprove: Function, onReject: Function, onCancel: Function, onRevise: Function, onRetry: Function },
  setup(props) {
    return () => {
      const { d, s, busy } = props, can = d.capabilities || {};
      const spec = d.latest_spec;
      const stop = busy || !!s.editor;
      if (!["approve", "revise", "reject", "cancel", "retry"].some(k => can["can_" + k])) return null;
      return h("div", { class: "actionbar", role: "region", "aria-label": "Run actions" }, [
        s.reviseOpen && can.can_revise ? h("form", { class: "revise", onSubmit: event => { event.preventDefault(); props.onRevise(); } }, [
          field("Revision notes (what should change?)", area(s, "notes", { id: "revise-notes", required: true, rows: 3 })),
          h("div", { class: "actions" }, [button("Send revision", null, { type: "submit", kind: "primary", disabled: stop || !s.notes.trim() }),
            button("Close", () => { s.reviseOpen = false; })])
        ]) : null,
        s.retryOpen && can.can_retry ? h("form", { class: "revise", onSubmit: event => { event.preventDefault(); props.onRetry(); } }, [
          h("h3", "Retry with a new run"), ...modelFields(props.providers),
          h("div", { class: "actions" }, [button("Start retry", null, { type: "submit", kind: "primary", disabled: busy }), button("Close", () => { s.retryOpen = false; })])
        ]) : null,
        h("div", { class: "actions bar" }, [
          can.can_approve && spec ? button(`Approve v${spec.version} · ${shortHash(spec.hash)}`, props.onApprove, { kind: "primary", icon: "check", disabled: stop, title: "Approves exactly this version (a)" }) : null,
          can.can_revise ? button("Revise", () => { s.reviseOpen = !s.reviseOpen; if (s.reviseOpen) s.retryOpen = false; }, { disabled: stop, pressed: s.reviseOpen, title: "Ask for changes (r)" }) : null,
          can.can_reject ? button("Reject", props.onReject, { kind: "danger", disabled: stop }) : null,
          can.can_cancel ? button("Cancel run", props.onCancel, { kind: "danger quiet", disabled: busy }) : null,
          can.can_retry ? button("Retry…", () => { s.retryOpen = !s.retryOpen; if (s.retryOpen) s.reviseOpen = false; }, { disabled: busy, pressed: s.retryOpen }) : null
        ])
      ]);
    };
  }
};
