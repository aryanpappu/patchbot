// Repositories page (M24): allowlist cards, add/edit, audit and remove.
import { block, button, empty, errorPanel, loading, panel } from "../ui.js";
import { useTask } from "../composables/useTask.js";
import { repoCard } from "./RepoCard.js";
import { newRepoEditor, repoEditor, settingsFrom } from "./RepoEditor.js";

const { h, reactive, onMounted } = window.Vue;

export const RepoPage = {
  props: { ctx: { type: Object, required: true } },
  setup(props) {
    const { api, toast, confirm } = props.ctx;
    const s = reactive({ ready: false, list: [], container: false, editor: null, audit: null });
    const task = useTask();

    async function load() {
      const result = await api.request("/api/repos");
      s.list = result.repos; s.container = !!result.container; s.ready = true;
    }
    onMounted(() => task.run(load));
    function save() {
      const e = s.editor;
      void task.run(async () => {
        const settings = settingsFrom(e);
        const result = e.mode === "add" ? await api.send("/api/repos", { name: e.name.trim(), settings })
          : await api.send("/api/repos/" + e.name, { version: e.version, settings }, "PUT");
        s.editor = null;
        toast.push((e.mode === "add" ? "Repository added." : "Repository saved.") + (result.github ? " GitHub check: " + result.github.message : ""));
        await load();
      });
    }
    async function remove(r) {
      const ok = await confirm.ask({ title: `Remove ${r.name}?`, confirmLabel: "Remove repository", danger: true,
        message: "New runs for it stop immediately. This is refused while it has active runs; run history is kept." });
      if (!ok) return;
      await task.run(async () => {
        try { await api.request("/api/repos/" + r.name, { method: "DELETE" }); }
        finally { if (!api.state.locked) await load(); }
        toast.push(`Repository ${r.name} removed. Run history is kept.`);
      });
    }
    const audit = r => task.run(async () => { s.audit = { repo: r.name, rows: (await api.request(`/api/repos/${r.name}/audit`)).audit }; });
    const handlers = { edit: r => { s.editor = newRepoEditor(r); }, remove, audit };

    return () => h("div", [
      h("h1", "Repositories"),
      h("p", "Repositories from the config file are read-only here. Changes apply to new runs; a run in flight keeps the settings it started with."),
      task.state.error ? errorPanel(task.state.error, () => task.run(load)) : null,
      s.editor ? repoEditor(s.editor, s.container, task.state.busy, save, () => { s.editor = null; })
        : button("Add repository", () => { s.editor = newRepoEditor(null); }, { kind: "primary", icon: "plus", disabled: task.state.busy || !s.ready }),
      !s.ready && !task.state.error ? loading("Loading repositories…") : null,
      s.ready && !s.list.length ? empty("No repositories are configured.", "Add one so patchbot may open pull requests against it.") : null,
      s.list.length ? h("ul", { class: "cards" }, s.list.map(r => repoCard(r, handlers, task.state.busy))) : null,
      s.audit ? panel("Audit: " + s.audit.repo, [s.audit.rows.length ? h("ol", s.audit.rows.map(a => h("li", { key: a.id }, [
        h("strong", `${a.action} · ${a.actor} · ${a.channel} · ${a.at}`),
        a.before ? h("details", [h("summary", "Before"), block(a.before, "Before")]) : null,
        a.after ? h("details", [h("summary", "After"), block(a.after, "After")]) : null
      ]))) : h("p", "No dashboard changes recorded."), button("Close", () => { s.audit = null; })]) : null
    ]);
  }
};
