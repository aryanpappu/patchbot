// A unified diff as text lines grouped per file. Lines are text nodes in a
// keyboard-scrollable panel; diffs over 500 lines sit behind "Show all".
import { parseDiff } from "../format.js";
import { button } from "../ui.js";

const { h, reactive } = window.Vue;
const LIMIT = 500;

export const DiffView = {
  props: { diff: { type: String, default: "" } },
  setup(props) {
    const local = reactive({ all: false });
    return () => {
      const files = parseDiff(props.diff);
      const total = files.reduce((n, f) => n + f.lines.length, 0);
      let budget = local.all ? Infinity : LIMIT;
      return h("div", { class: "diff" }, [
        files.length > 1 ? h("ul", { class: "filetree", "aria-label": "Changed files" }, files.map(f => h("li", { key: f.path }, [
          h("span", { class: "mono" }, f.path), h("span", { class: "add" }, ` +${f.adds}`), h("span", { class: "del" }, ` -${f.dels}`)]))) : null,
        h("div", { class: "output diff-lines mono", tabindex: 0, role: "region", "aria-label": "Diff" }, files.flatMap(f => {
          const shown = f.lines.slice(0, Math.max(0, budget));
          budget -= shown.length;
          return shown.map((line, i) => h("div", { class: "dl " + line.kind, key: f.path + i }, line.text || " "));
        })),
        !local.all && total > LIMIT ? button(`Show all ${total} lines`, () => { local.all = true; }) : null
      ]);
    };
  }
};
