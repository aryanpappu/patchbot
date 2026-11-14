// Structured spec editor. Saving creates a new version and clears approval.
import { area, button, field, input } from "../ui.js";

const { h } = window.Vue;
const lists = [["acceptance_criteria", "Acceptance criteria"], ["risks", "Risks"], ["test_plan", "Test plan"], ["open_questions", "Open questions"]];

export function newEditor(spec) {
  const e = { summary: spec.summary, files: spec.files.map(f => ({ ...f })) };
  for (const [key] of lists) e[key] = spec[key].join("\n");
  return e;
}

export const SpecEditor = {
  props: { editor: Object, busy: Boolean, canEdit: Boolean, onSave: Function, onDiscard: Function },
  setup(props) {
    return () => {
      const e = props.editor;
      return h("form", { class: "editor", onSubmit: event => { event.preventDefault(); props.onSave(); } }, [
        h("h3", "Edit specification"),
        field("Summary", area(e, "summary", { required: true })),
        ...lists.map(([key, label]) => field(label + " (one per line)", area(e, key))),
        h("fieldset", [h("legend", "Files likely to change"), ...e.files.map((file, index) => h("div", { class: "file-row", key: index }, [
          field("Path", input(file, "path", { required: true })), field("Change", input(file, "change", { required: true })),
          button(`Remove file ${index + 1}`, () => e.files.splice(index, 1), { disabled: props.busy || !props.canEdit })
        ])), button("Add file", () => e.files.push({ path: "", change: "" }), { disabled: props.busy || !props.canEdit })]),
        h("div", { class: "actions" }, [
          button("Save new version", null, { type: "submit", kind: "primary", disabled: props.busy || !props.canEdit }),
          button("Discard edit", props.onDiscard, { disabled: props.busy })
        ])
      ]);
    };
  }
};
