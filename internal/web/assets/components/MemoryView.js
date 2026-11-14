// Repository memory: review pending lessons, forget entries, add a lesson.
import { visible } from "../format.js";
import { area, block, button, empty, errorPanel, field, loading, select } from "../ui.js";
import { useTask } from "../composables/useTask.js";

const { h, reactive, onMounted } = window.Vue;

export const MemoryView = {
  props: { ctx: { type: Object, required: true } },
  setup(props) {
    const { api, toast, confirm, providers, badges } = props.ctx;
    const s = reactive({ ready: false, enabled: true, entries: [] });
    const filters = reactive({ repo: "", status: "pending" });
    const lesson = reactive({ repo: "", text: "" });
    const task = useTask();

    async function load() {
      const params = new URLSearchParams();
      if (filters.repo) params.set("repo", filters.repo);
      if (filters.status) params.set("status", filters.status);
      const body = await api.request("/api/memory?" + params);
      s.enabled = body.enabled; s.entries = body.entries || []; s.ready = true;
      void badges.refresh();
    }
    onMounted(() => task.run(async () => {
      await providers.load().catch(() => { /* Repositories fall back to the filter only. */ });
      lesson.repo = providers.state.repos[0] || "";
      await load();
    }));
    const decide = (entry, name) => task.run(async () => {
      // Bind the decision to the exact content hash shown to the operator.
      try { await api.send(`/api/memory/${entry.id}/${name}`, { content_hash: entry.content_hash }); }
      finally { if (!api.state.locked) await load(); }
      toast.push(`Memory ${entry.id}: ${name} accepted.`);
    });
    async function forget(entry) {
      if (await confirm.ask({ title: `Forget memory #${entry.id}?`, message: "The entry stops being used and its text is erased. This cannot be undone.", confirmLabel: "Forget", danger: true })) await decide(entry, "forget");
    }
    function add(event) {
      event.preventDefault();
      void task.run(async () => {
        try { await api.send("/api/memory", { repo: lesson.repo, text: lesson.text }); }
        finally { if (!api.state.locked) await load(); }
        lesson.text = "";
        toast.push("Operator lesson saved and approved.");
      });
    }
    const repoOptions = () => providers.state.repos.map(r => [r, r]);
    return () => h("div", [
      h("h1", "Repository memory"),
      h("p", "Memory is unverified context for the model. It never changes command policy, protected paths, repository scope or approval. Only approved lessons and fresh observations are retrieved."),
      s.enabled ? null : h("p", { class: "warning", role: "status" }, "Memory is disabled: runs neither retrieve nor write memory."),
      task.state.error ? errorPanel(task.state.error, () => task.run(load)) : null,
      h("form", { class: "filters", onSubmit: event => { event.preventDefault(); void task.run(load); } }, [
        field("Repository", select(filters, "repo", [["", "All repositories"], ...repoOptions()])),
        field("Status", select(filters, "status", [["", "All statuses"], ...["pending", "approved", "active", "rejected", "revoked"].map(v => [v, v])])),
        button("Show memory", null, { type: "submit", disabled: task.state.busy })
      ]),
      !s.ready && !task.state.error ? loading("Loading memory…") : null,
      s.ready && !s.entries.length ? empty("No memory entries match these filters.", "Pick another status, or add a lesson below.") : null,
      s.entries.length ? h("ul", { class: "cards" }, s.entries.map(e => h("li", { key: e.id, class: "card" }, [
        h("p", h("strong", `#${e.id} · ${e.repo} · ${e.kind} · ${e.status} · ${e.source}${e.run_id ? " · run " + e.run_id : ""}`)),
        block(visible(e.kind === "lesson" ? (e.text || "(forgotten: text erased)") : e.summary), "Memory text"),
        h("p", { class: "muted small mono" }, `Content hash ${e.content_hash}`),
        h("div", { class: "actions" }, [
          e.kind === "lesson" && e.status === "pending" ? button("Approve", () => decide(e, "approve"), { disabled: task.state.busy }) : null,
          e.kind === "lesson" && e.status === "pending" ? button("Reject", () => decide(e, "reject"), { disabled: task.state.busy }) : null,
          e.status !== "revoked" ? button("Forget", () => forget(e), { kind: "danger", disabled: task.state.busy }) : null
        ])
      ]))) : null,
      h("form", { class: "panel", onSubmit: add }, [
        h("h2", "Add an operator lesson (approved immediately)"),
        field("Repository", select(lesson, "repo", repoOptions())),
        field("Lesson (up to 600 bytes)", area(lesson, "text", { required: true, maxlength: 600 })),
        button("Add lesson", null, { type: "submit", kind: "primary", disabled: task.state.busy || !lesson.text.trim() || !lesson.repo })
      ])
    ]);
  }
};
