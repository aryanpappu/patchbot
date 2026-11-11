/* All untrusted values are Vue text children. Render functions need no eval. */
(() => {
  "use strict";
  if (!window.Vue) return;
  const initial = JSON.parse(document.getElementById("state").textContent);
  const { createApp, h, reactive, onMounted, onUnmounted } = window.Vue;
  const active = ["queued", "running", "awaiting_approval", "needs_info", "approved"];
  const statuses = [...active, "succeeded", "failed", "cancelled", "rejected"];
  const storageKey = "patchbot.ui-token";
  const text = value => value == null ? "unknown" : String(value);
  const money = value => value == null ? "unknown" : `$${(value / 1000000).toFixed(4)}`;
  const human = value => String(value || "pending").replaceAll("_", " ");
  const block = value => h("pre", { class: "output", tabindex: 0 }, text(value));
  const link = (url, label) => {
    try {
      const parsed = new URL(url);
      if (["http:", "https:"].includes(parsed.protocol)) {
        return h("a", { href: parsed.href, target: "_blank", rel: "noopener noreferrer" }, label);
      }
    } catch { /* Invalid external links remain text. */ }
    return h("span", label);
  };
  const button = (label, click, disabled = false) => h("button", { type: "button", onClick: click, disabled }, label);
  const field = (label, control) => h("label", { class: "field" }, [h("span", label), control]);
  const input = (object, key, attributes = {}) => h("input", {
    ...attributes, value: object[key], onInput: event => { object[key] = event.target.value; }
  });
  const area = (object, key, attributes = {}) => h("textarea", {
    rows: 4, ...attributes, value: object[key], onInput: event => { object[key] = event.target.value; }
  });
  const select = (object, key, options, change) => h("select", {
    value: object[key], onChange: event => { object[key] = event.target.value; if (change) change(); }
  }, options.map(([value, label]) => h("option", { value }, label)));
  const chip = status => h("span", { class: "chip " + (statuses.includes(status) ? status : "") }, human(status));
  const panel = (title, children) => h("section", { class: "panel" }, [h("h2", title), ...children]);
  const lines = value => value.split("\n").map(line => line.trim()).filter(Boolean);
  const specText = spec => (spec?.body_md || "").replace(/\n<!-- patchbot:spec-data [A-Za-z0-9+/=]+ -->\n$/, "");

  // A linear replacement diff: common prefix/suffix stay as context; changed
  // lines are shown in full. No quadratic work on model-generated documents.
  function lineDiff(before, after) {
    const a = before.split("\n"), b = after.split("\n");
    let start = 0, end = 0;
    while (start < a.length && start < b.length && a[start] === b[start]) start++;
    while (end < a.length - start && end < b.length - start && a[a.length - end - 1] === b[b.length - end - 1]) end++;
    if (start === a.length && start === b.length) return "No changes.";
    return [
      ...a.slice(Math.max(0, start - 3), start).map(line => "  " + line),
      ...a.slice(start, a.length - end).map(line => "- " + line),
      ...b.slice(start, b.length - end).map(line => "+ " + line),
      ...b.slice(b.length - end, b.length - end + 3).map(line => "  " + line)
    ].join("\n");
  }

  const app = createApp({
    delimiters: ["[[", "]]"],
    setup() {
      let token = "";
      try { token = sessionStorage.getItem(storageKey) || ""; } catch { /* The login form explains storage failures. */ }
      const s = reactive({
        locked: initial.auth_required && !token, tokenInput: "", busy: false, ready: false,
        error: "", notice: "", providers: [], repos: [], models: [], modelWarning: "",
        runs: [], cursor: "", awaiting: null, detail: initial.initial || null, artifacts: [], events: [],
        streamStatus: "Connecting", version: "", compare: "", editor: null, notes: ""
      });
      const query = new URLSearchParams(location.search);
      const filters = reactive({ repo: query.get("repo") || "", issue: query.get("issue") || "", status: query.get("status") || "" });
      const form = reactive({ repo: "", issue: "", mode: "fix", budget: "2", dry: true, provider: "", model: "", separate: false, refine: "" });
      let stopStream = null, refreshTimer = null, pollTimer = null, lastID = 0, modelRequest = 0;
      let refreshing = null, disposed = false;

      function forgetToken() {
        token = "";
        try { sessionStorage.removeItem(storageKey); } catch { /* No persistent fallback. */ }
        s.locked = true;
        s.ready = false;
        s.detail = null;
        s.runs = []; s.events = []; s.artifacts = []; s.editor = null;
        s.providers = []; s.repos = []; s.models = []; s.awaiting = null;
        lastID = 0;
        modelRequest++;
        if (stopStream) stopStream.abort();
      }
      function headers(extra = {}) {
        return token ? { ...extra, Authorization: "Bearer " + token } : extra;
      }
      async function request(path, options = {}, plain = false) {
        const response = await fetch(path, { ...options, credentials: "same-origin", headers: headers(options.headers) });
        if (response.status === 401) {
          forgetToken();
          throw new Error("Enter the UI token to continue.");
        }
        if (!response.ok) {
          let message = `Request failed (${response.status}).`;
          try { const body = await response.json(); message = body.error?.message || message; } catch { /* Use status if the response is not JSON. */ }
          throw new Error(message);
        }
        return plain ? response.text() : response.json();
      }
      const send = (path, body, method = "POST") => request(path, {
        method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body)
      });
      async function work(task) {
        s.error = ""; s.notice = ""; s.busy = true;
        try { await task(); } catch (error) { s.error = error.message; }
        finally { s.busy = false; }
      }
      async function loadModels(reset = false) {
        const serial = ++modelRequest;
        s.models = []; s.modelWarning = "Loading models…";
        if (reset) {
          form.model = s.providers.find(p => p.id === form.provider)?.default_model || "";
          form.refine = ""; form.separate = false;
        }
        try {
          const list = await request(`/api/providers/${encodeURIComponent(form.provider)}/models`);
          if (serial !== modelRequest) return;
          s.models = list.models || [];
          s.modelWarning = list.warning || (!list.live ? "Live listing failed. Choose a configured model or type a model ID." : "");
        } catch (error) {
          if (serial === modelRequest) s.modelWarning = error.message + " Type a model ID to continue.";
        }
      }
      function modelFields(refine = false) {
        return [
          field("Provider", select(form, "provider", s.providers.map(p => [p.id, p.id + (p.available ? "" : " (unavailable)")]), () => loadModels(true))),
          field("Model (choose or type an ID)", input(form, "model", { list: "model-ids", required: true, maxlength: 512 })),
          h("datalist", { id: "model-ids" }, s.models.map(m => h("option", { value: m.id }, m.display_name || m.id))),
          s.modelWarning ? h("p", { class: "warning", role: "status" }, s.modelWarning) : null,
          refine ? h("label", { class: "check" }, [h("input", { type: "checkbox", checked: form.separate, onChange: e => { form.separate = e.target.checked; } }), "Use a separate refine model"]) : null,
          refine && form.separate ? field("Refine model (same provider)", input(form, "refine", { list: "model-ids", required: true, maxlength: 512 })) : null
        ];
      }
      async function countAwaiting() {
        let cursor = "", count = 0;
        do {
          const q = new URLSearchParams({ status: "awaiting_approval", limit: "500" });
          if (cursor) q.set("cursor", cursor);
          const page = await request("/api/runs?" + q);
          count += page.runs.length;
          cursor = page.next_cursor;
        } while (cursor && !disposed && !s.locked);
        s.awaiting = count;
      }
      async function loadRuns(cursor = query.get("cursor") || "", apply = false) {
        const params = new URLSearchParams({ limit: "25" });
        for (const key of ["repo", "issue", "status"]) {
          const value = apply ? filters[key] : query.get(key);
          if (value) params.set(key, value);
        }
        if (cursor) params.set("cursor", cursor);
        const page = await request("/api/runs?" + params);
        s.runs = page.runs; s.cursor = page.next_cursor;
        for (const key of ["repo", "issue", "status", "cursor"]) {
          query.delete(key);
          if (params.has(key)) query.set(key, params.get(key));
        }
        history.replaceState(null, "", "/runs?" + params);
        await countAwaiting();
      }
      const can = action => !!s.detail?.capabilities?.[`can_${action}`];
      async function loadDetail(fresh = false) {
        while (refreshing) {
          await refreshing;
          if (!fresh) return;
        }
        refreshing = (async () => {
          const detail = await request(`/api/runs/${initial.run_id}`);
          s.detail = detail;
          const loaded = new Map(s.artifacts.map(a => [a.key, a.content]));
          const artifacts = await Promise.all((detail.artifacts || []).map(async a => {
            const key = `${a.kind}:${a.attempt}:${a.bytes}:${a.created_at}`;
            const content = loaded.has(key) ? loaded.get(key) : await request(
              `/api/runs/${initial.run_id}/artifacts/${encodeURIComponent(a.kind)}?attempt=${a.attempt}`, {}, true);
            return { ...a, key, content };
          }));
          s.artifacts = artifacts;
        })();
        try { await refreshing; } finally { refreshing = null; }
      }
      function scheduleRefresh() {
        if (refreshTimer) return;
        refreshTimer = setTimeout(() => {
          refreshTimer = null;
          if (!s.locked) loadDetail(true).catch(error => { s.error = error.message; });
        }, 200);
      }
      function consume(frame) {
        let id = "", type = "message";
        const data = [];
        for (const line of frame.split("\n")) {
          const colon = line.indexOf(":");
          const key = colon < 0 ? line : line.slice(0, colon);
          const value = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "");
          if (key === "id") id = value;
          if (key === "event") type = value;
          if (key === "data") data.push(value);
        }
        if (type === "end") {
          try {
            const end = JSON.parse(data.join("\n"));
            return Number.isSafeInteger(end.last_seq) && end.last_seq === lastID;
          } catch { return false; }
        }
        if (!data.length || !/^\d+$/.test(id) || !Number.isSafeInteger(Number(id)) || Number(id) <= lastID) return false;
        const payload = data.join("\n");
        let output = payload;
        try { output = JSON.stringify(JSON.parse(payload), null, 2); } catch { /* Render malformed data as text. */ }
        s.events.push({ id, type, output });
        if (s.events.length > 1000) s.events.shift();
        lastID = Number(id); // Advance only after consumption; reconnects replay anything missed.
        scheduleRefresh();
        return false;
      }
      async function follow(signal) {
        let delay = 1000;
        while (!signal.aborted && !s.locked && !disposed) {
          try {
            s.streamStatus = lastID ? "Reconnecting" : "Connecting";
            const extra = { Accept: "text/event-stream" };
            if (lastID) extra["Last-Event-ID"] = String(lastID);
            const response = await fetch(`/api/runs/${initial.run_id}/events`, {
              credentials: "same-origin", headers: headers(extra), signal
            });
            if (response.status === 401) { forgetToken(); throw new Error("Enter the UI token to resume the log."); }
            if (!response.ok || !response.body) throw new Error(`Live log unavailable (${response.status}).`);
            s.streamStatus = "Live";
            const reader = response.body.getReader(), decoder = new TextDecoder();
            let pending = "", frame = [], complete = false;
            try {
              while (!signal.aborted && !complete) {
                const part = await reader.read();
                if (part.done) break;
                pending += decoder.decode(part.value, { stream: true });
                let newline;
                while ((newline = pending.indexOf("\n")) >= 0) {
                  const line = pending.slice(0, newline).replace(/\r$/, "");
                  pending = pending.slice(newline + 1);
                  if (line === "") {
                    const before = lastID;
                    complete = consume(frame.join("\n")); frame = [];
                    if (lastID > before) delay = 1000;
                    if (complete) break;
                  }
                  else frame.push(line);
                }
              }
            } finally { await reader.cancel(); reader.releaseLock(); }
            await loadDetail();
            // Only an explicit end frame proves terminal replay was drained.
            // Every other EOF reconnects from the last consumed persisted ID.
            if (complete) {
              s.streamStatus = "Complete"; return;
            }
            s.streamStatus = "Reconnecting";
          } catch (error) {
            if (signal.aborted || disposed) return;
            s.streamStatus = error.message + " Reconnecting…";
          }
          if (s.locked) return;
          await new Promise(resolve => setTimeout(resolve, delay));
          delay = Math.min(delay * 2, 10000);
        }
      }
      async function boot() {
        const providers = await request("/api/providers");
        s.providers = providers.providers; s.repos = providers.repos;
        form.repo = s.repos[0] || "";
        form.provider = providers.defaults.provider;
        form.mode = providers.defaults.mode;
        form.model = providers.defaults.code_model || "";
        form.refine = providers.defaults.refine_model || "";
        form.separate = !!form.refine && form.refine !== form.model;
        form.budget = String(providers.defaults.max_cost_usd);
        if (initial.page === "detail") {
          await loadDetail();
          form.provider = s.detail.run.provider; form.model = s.detail.run.code_model;
          lastID = 0; s.events = [];
          if (stopStream) stopStream.abort();
          stopStream = new AbortController();
          void follow(stopStream.signal);
        } else if (initial.page === "runs") await loadRuns();
        s.ready = true;
        if (initial.page !== "runs") void loadModels();
      }
      const navigateRun = run => { location.assign(`/runs/${run.id}`); };
      function start(event) {
        event.preventDefault();
        void work(async () => {
          const budget = Number(form.budget), issue = Number(form.issue);
          if (!Number.isSafeInteger(issue) || issue <= 0 || !Number.isFinite(budget) || budget <= 0 || !Number.isSafeInteger(Math.round(budget * 1000000))) {
            throw new Error("Enter a positive issue number and budget.");
          }
          const result = await send("/api/runs", {
            repo: form.repo, issue, mode: form.mode, provider: form.provider, code_model: form.model,
            refine_model: form.separate ? form.refine : form.model,
            max_cost_micros: Math.round(budget * 1000000), dry_run: form.dry
          });
          navigateRun(result.run);
        });
      }
      function action(name, body = {}) {
        if (!can(name) || s.busy) return;
        void work(async () => {
          let result;
          try { result = await send(`/api/runs/${initial.run_id}/${name}`, body); }
          finally { if (!s.locked) await loadDetail(true); }
          if (name === "retry") { navigateRun(result.run); return; }
          s.notice = human(name) + " accepted.";
          s.notes = "";
        });
      }
      function edit() {
        if (!can("edit") || s.busy) return;
        void work(async () => {
          const match = s.detail.latest_spec.body_md.match(/\n<!-- patchbot:spec-data ([A-Za-z0-9+/=]+) -->\n$/);
          if (!match) throw new Error("This specification has no editable structured fields.");
          const draft = JSON.parse(new TextDecoder().decode(Uint8Array.from(atob(match[1]), c => c.charCodeAt(0))));
          s.editor = { summary: draft.summary, files: draft.files || [] };
          for (const key of ["acceptance_criteria", "risks", "test_plan", "open_questions"]) s.editor[key] = (draft[key] || []).join("\n");
        });
      }
      function saveSpec(event) {
        event.preventDefault();
        if (!can("edit") || s.busy) return;
        void work(async () => {
          const draft = { summary: s.editor.summary, files: s.editor.files.map(file => ({ path: file.path, change: file.change })) };
          for (const key of ["acceptance_criteria", "risks", "test_plan", "open_questions"]) draft[key] = lines(s.editor[key]);
          try { await send(`/api/runs/${initial.run_id}/spec`, draft, "PUT"); }
          finally { if (!s.locked) await loadDetail(true); }
          s.editor = null; s.version = "";
          s.notice = "New specification version saved. Any prior approval is cleared.";
        });
      }
      function login(event) {
        event.preventDefault();
        void work(async () => {
          token = s.tokenInput.trim(); s.tokenInput = "";
          if (!token) throw new Error("Enter the UI token.");
          try { sessionStorage.setItem(storageKey, token); }
          catch { token = ""; throw new Error("Enable session storage to use token authentication in this tab."); }
          s.locked = false;
          await boot();
        });
      }
      function newView() {
        return [h("h1", "New run"), h("form", { class: "panel", onSubmit: start }, [
          field("Repository", select(form, "repo", s.repos.map(repo => [repo, repo]))),
          field("Issue number", input(form, "issue", { type: "number", min: 1, step: 1, required: true })),
          field("Mode", select(form, "mode", ["fix", "refine", "investigate"].map(mode => [mode, mode]))),
          ...modelFields(true),
          field("Run budget (USD)", input(form, "budget", { type: "number", min: "0.000001", step: "0.000001", required: true })),
          h("label", { class: "check" }, [h("input", { type: "checkbox", checked: form.dry, onChange: e => { form.dry = e.target.checked; } }), "Dry run — produce artifacts without publishing"]),
          h("button", { type: "submit", disabled: s.busy || !s.repos.length || !form.provider }, "Start run")
        ])];
      }
      function listView() {
        return [h("h1", "Runs"), h("p", { class: "badge" }, `Awaiting approval: ${s.awaiting == null ? "loading" : s.awaiting}`),
          h("form", { class: "filters", onSubmit: event => { event.preventDefault(); void work(() => loadRuns("", true)); } }, [
            field("Repository", select(filters, "repo", [["", "All repositories"], ...s.repos.map(repo => [repo, repo])])),
            field("Issue", input(filters, "issue", { type: "number", min: 1, step: 1 })),
            field("Status", select(filters, "status", [["", "All statuses"], ...statuses.map(status => [status, human(status)])])),
            h("button", { type: "submit", disabled: s.busy }, "Apply filters")
          ]),
          s.runs.length ? h("ul", { class: "runs" }, s.runs.map(run => h("li", { key: run.id, class: "panel" }, [
            h("h2", [h("a", { href: `/runs/${run.id}` }, `${run.repo} #${run.issue_number} · run ${run.id}`)]),
            h("p", run.issue_title), chip(run.status), h("p", `${human(run.phase)} · cost ${money(run.cost_micros)}${run.dry_run ? " · dry run" : ""}`),
            run.prurl ? link(run.prurl, "Pull request") : null
          ]))) : h("p", "No runs match these filters."),
          h("div", { class: "actions" }, [
            button("First page", () => work(() => loadRuns("")), s.busy || !query.get("cursor")),
            button("Older runs", () => work(() => loadRuns(s.cursor)), s.busy || !s.cursor)
          ])
        ];
      }
      function editorView() {
        const e = s.editor;
        return h("form", { class: "editor", onSubmit: saveSpec }, [
          field("Summary", area(e, "summary", { required: true })),
          ...[["acceptance_criteria", "Acceptance criteria"], ["risks", "Risks"], ["test_plan", "Test plan"], ["open_questions", "Open questions"]].map(([key, label]) => field(label + " (one per line)", area(e, key))),
          h("fieldset", [h("legend", "Files likely to change"), ...e.files.map((file, index) => h("div", { class: "file-row" }, [
            field("Path", input(file, "path", { required: true })), field("Change", input(file, "change", { required: true })),
            button(`Remove file ${index + 1}`, () => e.files.splice(index, 1), s.busy || !can("edit"))
          ])), button("Add file", () => e.files.push({ path: "", change: "" }), s.busy || !can("edit"))]),
          h("div", { class: "actions" }, [h("button", { type: "submit", disabled: s.busy || !can("edit") }, "Save new version"), button("Discard edit", () => { s.editor = null; }, s.busy)])
        ]);
      }
      function specView() {
        const d = s.detail, versions = d.spec_versions || [];
        if (!d.latest_spec) return panel("Specification", [h("p", "No specification yet.")]);
        const current = versions.find(v => String(v.version) === s.version) || d.latest_spec;
        const previous = versions.find(v => String(v.version) === s.compare) || versions[Math.max(0, versions.indexOf(current) - 1)];
        const options = versions.map(v => [String(v.version), `Version ${v.version} · ${v.source} · ${v.created_at}`]);
        return panel("Specification", [
          field("View version", select(s, "version", [["", "Latest version"], ...options])),
          h("p", `Version ${current.version} · SHA-256 ${current.hash}`),
          block(specText(current)),
          h("p", "Specifications are displayed as plain text. HTML and links are never executed."),
          field("Compare against", select(s, "compare", [["", "Previous version"], ...options])),
          h("details", [h("summary", `Changes from version ${previous.version} to ${current.version}`), block(lineDiff(specText(previous), specText(current)))]),
          button("Edit latest specification", edit, s.busy || !can("edit") || !!s.editor),
          s.editor ? editorView() : null
        ]);
      }
      function detailView() {
        const d = s.detail, run = d.run;
        const disabled = s.busy || !!s.editor;
        const phases = ["preflight", "refine", "clone", "baseline", "code", "verify", "publish"];
        return [h("h1", `${run.repo} #${run.issue_number} · run ${run.id}`), h("p", run.issue_title),
          h("div", { class: "actions" }, [chip(run.status), run.dry_run ? h("span", "Dry run") : null, run.prurl ? link(run.prurl, "Open pull request") : null]),
          h("p", `${run.mode} · ${run.provider} · refine: ${run.refine_model} · code: ${run.code_model}`),
          run.retry_of ? h("p", ["Retry of ", h("a", { href: `/runs/${run.retry_of}` }, `run ${run.retry_of}`)]) : null,
          h("dl", { class: "counters" }, [
            ...[["Input tokens", text(run.input_tokens)], ["Output tokens", text(run.output_tokens)], ["Cache read", text(run.cache_read_tokens)], ["Cache write", text(run.cache_write_tokens)], ["Cost (USD)", money(run.cost_micros)]].flatMap(([label, value]) => [h("dt", label), h("dd", value)])
          ]),
          run.error_kind ? h("p", { class: "error" }, `${run.error_kind}: ${run.error_message}`) : null,
          run.summary ? block(run.summary) : null,
          panel("Phase timeline", [h("ol", { class: "timeline" }, phases.map(phase => {
            const steps = (d.steps || []).filter(step => step.phase === phase);
            const latest = steps[steps.length - 1];
            const state = phase === run.phase ? human(run.status) : latest ? human(latest.status) : "not recorded";
            return h("li", [h("strong", human(phase)), ` · ${state}`, latest ? h("small", ` ${latest.started_at}`) : null]);
          })), h("details", [h("summary", "Recorded steps"), h("ul", (d.steps || []).map(step => h("li", `${step.seq} · ${step.phase} · ${step.kind} ${step.tool} · ${step.status}${step.exit_code == null ? "" : " · exit " + step.exit_code}`)))])]),
          specView(),
          panel("Run controls", [
            h("div", { class: "actions" }, [
              button("Approve", () => action("approve"), disabled || !can("approve")),
              button("Reject", () => action("reject"), disabled || !can("reject")),
              button("Cancel", () => action("cancel"), s.busy || !can("cancel"))
            ]),
            h("form", { onSubmit: event => { event.preventDefault(); action("revise", { notes: s.notes }); } }, [
              field("Revision notes", area(s, "notes", { required: true, disabled: !can("revise") })),
              h("button", { type: "submit", disabled: disabled || !can("revise") || !s.notes.trim() }, "Revise")
            ]),
            h("form", { onSubmit: event => { event.preventDefault(); action("retry", { provider: form.provider, model: form.model }); } }, [
              h("h3", "Retry with a new run"), ...modelFields(),
              h("button", { type: "submit", disabled: s.busy || !can("retry") }, "Retry")
            ])
          ]),
          panel("Diff", s.artifacts.filter(a => a.kind === "diff").map(a => block(a.content)).concat(s.artifacts.some(a => a.kind === "diff") ? [] : [h("p", "No diff recorded yet.")])),
          panel("Test output", [h("p", `Harness: ${run.tests_status || "not recorded"} · ${run.test_attempts} attempts`),
            ...s.artifacts.filter(a => ["test_output", "baseline"].includes(a.kind)).map(a => h("details", [h("summary", `${human(a.kind)} · attempt ${a.attempt}`), block(a.content)]))]),
          panel("Pull request preview", s.artifacts.filter(a => ["pr_title", "pr_body"].includes(a.kind)).map(a => h("div", [h("h3", human(a.kind)), block(a.content)]))),
          panel("Live log", [h("p", { role: "status" }, `${s.streamStatus} · last event ${lastID} · showing the latest 1,000 events`),
            h("ol", { class: "events", tabindex: 0, "aria-label": "Run event log" }, s.events.map(event => h("li", { key: event.id }, [h("strong", `${event.id} · ${event.type}`), block(event.output)])))])
        ];
      }
      onMounted(() => {
        if (!s.locked) void work(boot);
        pollTimer = setInterval(() => {
          if (s.locked || !s.ready || s.busy || disposed) return;
          const refresh = initial.page === "detail" ? loadDetail : initial.page === "runs" ? loadRuns : null;
          if (refresh) void refresh().catch(error => { s.error = error.message; });
        }, 15000);
      });
      onUnmounted(() => {
        disposed = true;
        if (stopStream) stopStream.abort();
        clearInterval(pollTimer); clearTimeout(refreshTimer);
      });
      return () => h("div", [
        s.error ? h("p", { class: "error", role: "alert" }, s.error) : null,
        s.notice ? h("p", { class: "notice", role: "status" }, s.notice) : null,
        s.locked ? h("form", { class: "panel", onSubmit: login }, [h("h1", "Unlock patchbot"),
          h("p", "Enter PATCHBOT_UI_TOKEN once for this tab. It is kept only in session storage."),
          field("UI token", input(s, "tokenInput", { type: "password", required: true, autocomplete: "off", autofocus: true })),
          h("button", { type: "submit", disabled: s.busy }, "Unlock")
        ]) : [initial.auth_required ? button("Lock this tab", forgetToken) : null,
          !s.ready ? h("p", { role: "status" }, "Loading…") : initial.page === "new" ? newView() : initial.page === "detail" ? detailView() : listView()
        ]
      ]);
    }
  });
  app.config.compilerOptions.delimiters = ["[[", "]]"];
  app.mount("#app");
})();
