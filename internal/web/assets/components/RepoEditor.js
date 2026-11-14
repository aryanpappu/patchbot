// Add / edit repository form. Commands are one JSON argv array per line, never a
// shell string; blank image and PATH are omitted so the repo inherits [executor].
import { lines } from "../format.js";
import { area, button, field, input } from "../ui.js";
import { argvText } from "./RepoCard.js";

const { h } = window.Vue;

export const newRepoEditor = entry => ({
  mode: entry ? "edit" : "add", name: entry?.name || "", version: entry?.version || 0,
  commands: argvText(entry?.settings.commands), test: entry?.settings.test_command || "",
  protectedPaths: (entry?.settings.protected || []).join("\n"), image: entry?.settings.image || "",
  path: (entry?.settings.path || []).join("\n")
});

export function parseCommands(value) {
  const commands = {};
  for (const line of lines(value)) {
    const cut = line.indexOf(":");
    if (cut < 1) throw new Error("Write each command as: id: [\"program\", \"arg\", ...]");
    let argv;
    try { argv = JSON.parse(line.slice(cut + 1)); } catch { argv = null; }
    if (!Array.isArray(argv) || !argv.length || !argv.every(a => typeof a === "string")) {
      throw new Error("Command " + line.slice(0, cut).trim() + " must be a JSON array of strings (argv, no shell).");
    }
    commands[line.slice(0, cut).trim()] = argv;
  }
  return commands;
}

export function settingsFrom(e) {
  const settings = { commands: parseCommands(e.commands), test_command: e.test.trim(), protected: lines(e.protectedPaths) };
  if (e.image.trim()) settings.image = e.image.trim();
  if (lines(e.path).length) settings.path = lines(e.path);
  return settings;
}

export function repoEditor(e, container, busy, onSave, onCancel) {
  return h("form", { class: "panel", onSubmit: event => { event.preventDefault(); onSave(); } }, [
    h("h2", e.mode === "add" ? "Add repository" : "Edit " + e.name),
    h("p", "Commands run without a shell, inside the sandbox, with no network. They decide what executes on run code, so review them like code."),
    e.mode === "add" ? field("Repository (owner/name)", input(e, "name", { required: true, maxlength: 140, autocomplete: "off" })) : null,
    field("Commands (one per line: id: JSON argv array)", area(e, "commands", { rows: 5, spellcheck: false })),
    field("Test command ID", input(e, "test", { maxlength: 32 })),
    field("Extra protected paths (one per line; .github/workflows is always protected)", area(e, "protectedPaths", { rows: 3, spellcheck: false })),
    container ? field("Container image (name@sha256:digest)", input(e, "image", { maxlength: 512, spellcheck: false })) : null,
    container ? field("Container PATH (one directory per line)", area(e, "path", { rows: 2, spellcheck: false })) : null,
    h("div", { class: "actions" }, [button(e.mode === "add" ? "Add repository" : "Save changes", null, { type: "submit", kind: "primary", disabled: busy }), button("Cancel", onCancel)])
  ]);
}
