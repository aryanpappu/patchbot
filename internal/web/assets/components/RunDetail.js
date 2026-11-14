// Run detail: header, phase stepper, budget, tabs (Spec / Live log / Changes)
// and the sticky action bar. Lifecycle rules stay on the server.
import { decodeSpec, human, runHref, shortHash } from "../format.js";
import { block, chip, errorPanel, link, loading } from "../ui.js";
import { useRunStream } from "../composables/useRunStream.js";
import { useTask } from "../composables/useTask.js";
import { ActionBar } from "./ActionBar.js";
import { budgetMeters } from "./BudgetMeters.js";
import { changesView } from "./ChangesView.js";
import { LogStream } from "./LogStream.js";
import { phaseStepper } from "./PhaseStepper.js";
import { newEditor } from "./SpecEditor.js";
import { SpecView } from "./SpecView.js";
import { tabList, tabPanel } from "./Tabs.js";

const { h, reactive, nextTick, onMounted, onUnmounted } = window.Vue;
const tabs = [{ id: "spec", label: "Spec" }, { id: "log", label: "Live log" }, { id: "changes", label: "Changes" }];

export const RunDetail = {
  props: { ctx: { type: Object, required: true } },
  setup(props) {
    const { ctx } = props, { api, toast, confirm, providers } = ctx;
    const id = ctx.initial.run_id;
    const hashTab = location.hash.slice(1);
    const s = reactive({ ready: false, detail: ctx.initial.initial || null, artifacts: [], editor: null, notes: "", reviseOpen: false, retryOpen: false,
      tab: tabs.some(t => t.id === hashTab) ? hashTab : "spec" });
    const task = useTask();
    let refreshing = null, refreshTimer = null, pollTimer = null, redirect = null, disposed = false;

    async function loadDetail(fresh = false) {
      while (refreshing) { await refreshing; if (!fresh) return; }
      refreshing = (async () => {
        const detail = await api.request(`/api/runs/${id}`);
        const loaded = new Map(s.artifacts.map(a => [a.key, a.content]));
        const artifacts = await Promise.all((detail.artifacts || []).map(async a => {
          const key = `${a.kind}:${a.attempt}:${a.bytes}:${a.created_at}`;
          const content = loaded.has(key) ? loaded.get(key) : await api.request(`/api/runs/${id}/artifacts/${encodeURIComponent(a.kind)}?attempt=${a.attempt}`, {}, true);
          return { ...a, key, content };
        }));
        if (disposed) return;
        s.detail = detail; s.artifacts = artifacts; s.ready = true;
      })();
      try { await refreshing; } finally { refreshing = null; }
    }
    function scheduleRefresh() {
      if (refreshTimer) return;
      refreshTimer = setTimeout(() => {
        refreshTimer = null;
        if (!api.state.locked && !disposed) loadDetail(true).catch(error => { task.state.error = error.message; });
      }, 200);
    }
    const stream = useRunStream(api, id, scheduleRefresh, () => loadDetail());
    const can = name => !!s.detail?.capabilities?.["can_" + name];
    const navigate = run => { location.assign(runHref(run.id)); };

    function act(name, body = {}) {
      if (!can(name) || task.state.busy) return Promise.resolve();
      return task.run(async () => {
        let result;
        try { result = await api.send(`/api/runs/${id}/${name}`, body); }
        finally { if (!api.state.locked) await loadDetail(true); }
        if (name === "retry") { navigate(result.run); return; }
        toast.push(human(name) + " accepted.");
        s.notes = ""; s.reviseOpen = false;
      });
    }
    async function approve() {
      const spec = s.detail?.latest_spec;
      if (!can("approve") || !spec) return;
      const ok = await confirm.ask({ title: `Approve specification v${spec.version}?`, confirmLabel: `Approve v${spec.version} · ${shortHash(spec.hash)}`,
        message: "patchbot will start changing code for exactly this version. Editing it later clears the approval.",
        details: [["Version", String(spec.version)], ["Hash", shortHash(spec.hash)], ["SHA-256", spec.hash]] });
      // Send exactly the version and hash the dialog showed. If the spec changed
      // meanwhile the server answers 409 spec_changed: act() shows the error and
      // reloads the run, and nothing is retried automatically.
      if (ok) await act("approve", { spec_version: spec.version, spec_hash: spec.hash });
    }
    async function reject() {
      if (!can("reject")) return;
      if (await confirm.ask({ title: "Reject this specification?", message: "The run stops. You can retry later to start over.", confirmLabel: "Reject", danger: true })) await act("reject");
    }
    async function cancel() {
      if (!can("cancel")) return;
      if (await confirm.ask({ title: "Cancel this run?", message: "Work in progress is stopped. Nothing already published is undone.", confirmLabel: "Cancel run", danger: true })) await act("cancel");
    }
    function edit() {
      const spec = decodeSpec(s.detail?.latest_spec);
      if (!can("edit") || task.state.busy) return;
      if (!spec) { task.state.error = "This specification has no editable structured fields."; return; }
      task.state.error = ""; s.editor = newEditor(spec);
    }
    function saveSpec() {
      if (!can("edit") || task.state.busy) return Promise.resolve();
      return task.run(async () => {
        const e = s.editor, lines = v => String(v).split("\n").map(x => x.trim()).filter(Boolean);
        const draft = { summary: e.summary, files: e.files.map(f => ({ path: f.path, change: f.change })) };
        for (const key of ["acceptance_criteria", "risks", "test_plan", "open_questions"]) draft[key] = lines(e[key]);
        try { await api.send(`/api/runs/${id}/spec`, draft, "PUT"); }
        finally { if (!api.state.locked) await loadDetail(true); }
        s.editor = null;
        toast.push("New specification version saved. Any prior approval is cleared.");
      });
    }
    const retry = () => act("retry", { provider: providers.form.provider, model: providers.form.model });
    function pickTab(tab) { s.tab = tab; try { history.replaceState(null, "", "#" + tab); } catch { /* Hash is a convenience. */ } }

    onMounted(() => {
      ctx.shortcuts.a = () => void approve();
      ctx.shortcuts.r = () => { if (can("revise")) { s.reviseOpen = true; s.retryOpen = false; void nextTick(() => document.getElementById("revise-notes")?.focus()); } };
      void task.run(async () => {
        await loadDetail();
        providers.form.provider = s.detail.run.provider; providers.form.model = s.detail.run.code_model;
        stream.start();
        void providers.load().then(() => { providers.form.provider = s.detail.run.provider; providers.form.model = s.detail.run.code_model; return providers.loadModels(); }).catch(() => { /* Retry can still type a model. */ });
      });
      pollTimer = setInterval(() => {
        if (api.state.locked || !s.ready || task.state.busy || s.editor) return;
        loadDetail().catch(error => { task.state.error = error.message; });
      }, 15000);
    });
    onUnmounted(() => {
      disposed = true; stream.dispose();
      delete ctx.shortcuts.a; delete ctx.shortcuts.r;
      clearInterval(pollTimer); clearTimeout(refreshTimer); clearTimeout(redirect);
    });

    return () => {
      const d = s.detail;
      if (!d) return h("div", [task.state.error ? errorPanel(task.state.error, () => task.run(() => loadDetail(true))) : loading("Loading run…")]);
      const run = d.run;
      return h("div", { class: "detail" }, [
        h("p", { class: "crumbs" }, [h("a", { href: "/runs" }, "Runs"), " / ", `${run.repo} #${run.issue_number}`]),
        h("h1", `${run.repo} #${run.issue_number}`), h("p", { class: "lead" }, run.issue_title),
        h("div", { class: "actions" }, [chip(run.status), run.dry_run ? h("span", { class: "tag" }, "Dry run") : null, run.prurl ? link(run.prurl, "Open pull request") : null]),
        h("p", { class: "meta" }, `run ${run.id} · ${run.mode} · ${run.provider} · refine: ${run.refine_model} · code: ${run.code_model}`),
        run.retry_of ? h("p", ["Retry of ", h("a", { href: runHref(run.retry_of) }, `run ${run.retry_of}`)]) : null,
        d.dashboard_issue ? h("p", { class: "meta" }, `Requested from the dashboard by ${d.dashboard_issue.requested_by}`) : null,
        task.state.error ? errorPanel(task.state.error) : null,
        phaseStepper(run), budgetMeters(run),
        run.summary ? h("details", [h("summary", "Run summary"), block(run.summary, "Run summary")]) : null,
        tabList("Run sections", tabs, s.tab, pickTab),
        s.tab === "spec" ? tabPanel("spec", [h(SpecView, { d, editor: s.editor, busy: task.state.busy, canEdit: can("edit"), onEdit: edit, onSave: saveSpec, onDiscard: () => { s.editor = null; } })]) : null,
        s.tab === "log" ? tabPanel("log", [h(LogStream, { stream })]) : null,
        s.tab === "changes" ? tabPanel("changes", [changesView(d, s.artifacts)]) : null,
        h(ActionBar, { d, s, busy: task.state.busy, providers, onApprove: approve, onReject: reject, onCancel: cancel, onRevise: () => act("revise", { notes: s.notes }), onRetry: retry })
      ]);
    };
  }
};
