// New request: start a run for an existing issue, or write a new request that
// patchbot opens as a GitHub issue (POST /api/issues, idempotent by key).
import { runHref } from "../format.js";
import { button, checkbox, errorPanel, field, input, area, link, loading, select } from "../ui.js";
import { useTask } from "../composables/useTask.js";
import { modelFields } from "./ModelFields.js";

const { h, reactive, onMounted, onUnmounted } = window.Vue;

const freshKey = () => {
  if (window.crypto?.randomUUID) return window.crypto.randomUUID();
  return Array.from(window.crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, "0")).join("");
};
const validBudget = budget => Number.isFinite(budget) && budget > 0 && Number.isSafeInteger(Math.round(budget * 1000000));

export const NewRequest = {
  props: { ctx: { type: Object, required: true } },
  setup(props) {
    const { api, toast, providers } = props.ctx;
    const { state: p, form } = providers;
    const draft = reactive({ write: false, title: "", body: "", start: true, key: "", print: "", created: null });
    const task = useTask();
    let redirect = null;

    onMounted(() => task.run(async () => { await providers.load(); void providers.loadModels(); }));
    onUnmounted(() => clearTimeout(redirect));
    const go = run => { location.assign(runHref(run.id)); };
    const modelBody = () => ({ mode: form.mode, provider: form.provider, code_model: form.model, refine_model: form.separate ? form.refine : form.model,
      max_cost_micros: Math.round(Number(form.budget) * 1000000), dry_run: form.dry });

    function startRun(event) {
      event.preventDefault();
      void task.run(async () => {
        const issue = Number(form.issue);
        if (!Number.isSafeInteger(issue) || issue <= 0 || !validBudget(Number(form.budget))) throw new Error("Enter a positive issue number and budget.");
        go((await api.send("/api/runs", { repo: form.repo, issue, ...modelBody() })).run);
      });
    }
    function submitIssue(event) {
      event.preventDefault();
      void task.run(async () => {
        if (!draft.title.trim()) throw new Error("Enter a title.");
        if (draft.start && !validBudget(Number(form.budget))) throw new Error("Enter a positive budget.");
        const payload = { repo: form.repo, title: draft.title, body: draft.body, start_run: draft.start };
        if (draft.start) Object.assign(payload, modelBody());
        // Same content keeps the key, so pressing the button again after a timeout cannot open a second issue.
        const print = JSON.stringify(payload);
        if (print !== draft.print || !draft.key) { draft.key = freshKey(); draft.print = print; }
        let result;
        try { result = await api.send("/api/issues", { ...payload, idempotency_key: draft.key }); }
        catch (error) { if (error.body?.issue) draft.created = error.body.issue; throw error; }
        draft.created = result.issue;
        toast.push(result.issue.created ? "Issue created." : "Issue already created for this request.");
        draft.title = ""; draft.body = ""; draft.key = ""; draft.print = "";
        if (result.run) redirect = setTimeout(() => go(result.run), 1500);
      });
    }
    const repoField = () => field("Repository", select(form, "repo", p.repos.map(r => [r, r])));
    const budget = () => field("Run budget (USD)", input(form, "budget", { type: "number", min: "0.000001", step: "0.000001", required: true }));
    const submit = (label, extra) => button(label, null, { type: "submit", kind: "primary", disabled: task.state.busy || !p.repos.length || extra });

    function existing() {
      return h("form", { class: "panel", onSubmit: startRun }, [
        repoField(), field("Issue number", input(form, "issue", { type: "number", min: 1, step: 1, required: true })),
        field("Mode", select(form, "mode", ["fix", "refine", "investigate"].map(m => [m, m]))),
        ...modelFields(providers, true), budget(), checkbox(form, "dry", "Dry run — produce artifacts without publishing"), submit("Start run", !form.provider)
      ]);
    }
    function written() {
      const made = draft.created;
      return h("form", { class: "panel", onSubmit: submitIssue }, [
        h("p", "patchbot opens a GitHub issue with this text and, if chosen, starts the refine step. Nothing changes in the repository until you approve the specification."),
        repoField(), field("Title", input(draft, "title", { required: true, maxlength: 256 })),
        field("Request", area(draft, "body", { rows: 8, maxlength: 65536 })),
        checkbox(draft, "start", "Start refine now"),
        ...(draft.start ? [field("Mode", select(form, "mode", ["fix", "refine"].map(m => [m, m]))), ...modelFields(providers, true), budget(),
          checkbox(form, "dry", "Dry run — produce artifacts without publishing")] : []),
        submit("Create issue", draft.start && !form.provider),
        made ? h("p", { role: "status" }, [`Created ${made.repo} #${made.number}: `, link(made.html_url, "open on GitHub")]) : null
      ]);
    }
    return () => h("div", [
      h("h1", "New request"),
      h("div", { class: "actions", role: "group", "aria-label": "Request type" }, [
        button("Existing issue", () => { draft.write = false; }, { pressed: !draft.write }),
        button("Write a new request", () => { draft.write = true; }, { pressed: draft.write })
      ]),
      task.state.error ? errorPanel(task.state.error) : null,
      !p.loaded && !task.state.error ? loading("Loading repositories and models…") : (draft.write ? written() : existing())
    ]);
  }
};
