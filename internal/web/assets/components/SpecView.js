// The Spec tab: structured view (criteria checklist, files, risks, test plan),
// version picker, version diff and the editor. Everything is text; the full
// plain text stays available because the structured view is a convenience.
import { decodeSpec, lineDiff, shortHash, specText } from "../format.js";
import { block, button, empty, field, icon, panel, select } from "../ui.js";
import { SpecEditor } from "./SpecEditor.js";

const { h, reactive } = window.Vue;

const list = (title, items, mark) => items.length ? h("section", { class: "spec-sec" }, [h("h3", title),
  h("ul", { class: mark ? "checks" : "plain" }, items.map((item, i) => h("li", { key: i }, [mark ? icon("check-circle", 16) : null, h("span", item)])))]) : null;

export const SpecView = {
  props: { d: Object, editor: Object, busy: Boolean, canEdit: Boolean, onEdit: Function, onSave: Function, onDiscard: Function },
  setup(props) {
    const local = reactive({ version: "", compare: "" });
    return () => {
      const d = props.d, versions = d.spec_versions || [];
      if (!d.latest_spec) return panel("Specification", [empty("No specification yet.", "It appears here once the refine step finishes.")]);
      const current = versions.find(v => String(v.version) === local.version) || d.latest_spec;
      const previous = versions.find(v => String(v.version) === local.compare) || versions[Math.max(0, versions.indexOf(current) - 1)] || current;
      const options = versions.map(v => [String(v.version), `Version ${v.version} · ${v.source} · ${v.created_at}`]);
      const spec = decodeSpec(current);
      return panel("Specification", [
        h("p", { class: "meta" }, ["Version ", h("strong", String(current.version)), " · SHA-256 ", h("span", { class: "mono", title: current.hash }, shortHash(current.hash)),
          current.version === d.latest_spec.version ? " · latest" : " · older version"]),
        spec ? h("div", { class: "spec-structured" }, [
          h("section", { class: "spec-sec" }, [h("h3", "Summary"), h("p", { class: "pre" }, spec.summary)]),
          list("Acceptance criteria", spec.acceptance_criteria, true),
          spec.files.length ? h("section", { class: "spec-sec" }, [h("h3", "Files likely to change"), h("ul", { class: "plain" }, spec.files.map((f, i) =>
            h("li", { key: i }, [h("span", { class: "mono" }, f.path), " — ", f.change])))]) : null,
          list("Risks", spec.risks), list("Test plan", spec.test_plan), list("Open questions", spec.open_questions)
        ]) : null,
        h("details", { open: !spec }, [h("summary", spec ? "Full specification text" : "Specification text"), block(specText(current), "Specification text")]),
        h("p", { class: "muted small" }, "Specifications are displayed as plain text. HTML and links are never executed."),
        versions.length > 1 ? h("div", { class: "filters" }, [
          field("View version", select(local, "version", [["", "Latest version"], ...options])),
          field("Compare against", select(local, "compare", [["", "Previous version"], ...options]))
        ]) : null,
        versions.length > 1 ? h("details", [h("summary", `Changes from version ${previous.version} to ${current.version}`), block(lineDiff(specText(previous), specText(current)), "Version diff")]) : null,
        props.editor ? h(SpecEditor, { editor: props.editor, busy: props.busy, canEdit: props.canEdit, onSave: props.onSave, onDiscard: props.onDiscard })
          : button("Edit latest specification", props.onEdit, { disabled: props.busy || !props.canEdit })
      ]);
    };
  }
};
