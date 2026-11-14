// One allowlisted repository. Config-file repositories are read-only. Commands,
// paths and audit values are text nodes.
import { button, chip as _chip, block } from "../ui.js";

const { h } = window.Vue;

export const argvText = commands => Object.keys(commands || {}).sort().map(id => id + ": " + JSON.stringify(commands[id])).join("\n");

export function repoCard(r, handlers, busy) {
  const dashboard = r.source === "dashboard";
  return h("li", { key: r.name, class: "card" }, [
    h("div", { class: "card-head" }, [h("h3", r.name), h("span", { class: "tag" }, dashboard ? "dashboard · v" + r.version : "config file (read-only)")]),
    h("dl", { class: "facts" }, [
      h("dt", "Commands"), h("dd", block(argvText(r.settings.commands) || "none", "Commands for " + r.name)),
      h("dt", "Test command"), h("dd", r.settings.test_command || "default (test) if defined"),
      h("dt", "Protected"), h("dd", { class: "mono" }, [...(r.builtin_protected || []), ...(r.settings.protected || [])].join(", ")),
      r.settings.image ? h("dt", "Image") : null, r.settings.image ? h("dd", { class: "mono" }, r.settings.image) : null,
      r.updated_by ? h("dt", "Last change") : null, r.updated_by ? h("dd", r.updated_by + " · " + r.updated_at) : null
    ]),
    h("div", { class: "actions" }, [
      dashboard ? button("Edit", () => handlers.edit(r), { disabled: busy }) : null,
      dashboard ? button("Remove", () => handlers.remove(r), { kind: "danger", disabled: busy }) : null,
      button("Audit", () => handlers.audit(r), { disabled: busy })
    ])
  ]);
}
