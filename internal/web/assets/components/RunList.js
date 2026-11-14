// All runs with filters and the API's opaque continuation cursor. Filters and
// cursor survive reload in the URL; filter edits are debounced 300 ms and the
// previous request is aborted.
import { human, statuses } from "../format.js";
import { button, empty, errorPanel, field, input, loading, select } from "../ui.js";
import { useTask } from "../composables/useTask.js";
import { runCard } from "./RunCard.js";

const { h, reactive, onMounted, onUnmounted } = window.Vue;

export const RunList = {
  props: { ctx: { type: Object, required: true } },
  setup(props) {
    const { api } = props.ctx;
    const query = new URLSearchParams(location.search);
    const filters = reactive({ repo: query.get("repo") || "", issue: query.get("issue") || "", status: query.get("status") || "" });
    const s = reactive({ ready: false, runs: [], cursor: "", first: query.get("cursor") || "", repos: [] });
    const task = useTask();
    let controller = null, debounce = null, timer = null;

    async function load(cursor) {
      if (controller) controller.abort();
      controller = new AbortController();
      const params = new URLSearchParams({ limit: "25" });
      for (const key of ["repo", "issue", "status"]) if (filters[key]) params.set(key, filters[key]);
      if (cursor) params.set("cursor", cursor);
      const page = await api.request("/api/runs?" + params, { signal: controller.signal });
      s.runs = page.runs; s.cursor = page.next_cursor; s.first = cursor; s.ready = true;
      history.replaceState(null, "", "/runs" + (params.size > 1 ? "?" + params : ""));
    }
    const go = cursor => task.run(() => load(cursor).catch(error => { if (error.name !== "AbortError") throw error; }));
    function filtered() { clearTimeout(debounce); debounce = setTimeout(() => void go(""), 300); }
    onMounted(() => {
      void go(s.first);
      api.request("/api/providers").then(body => { s.repos = body.repos; }).catch(() => { /* Filter falls back to typing. */ });
      timer = setInterval(() => { if (!task.state.busy && !api.state.locked) void go(s.first); }, 15000);
    });
    onUnmounted(() => { if (controller) controller.abort(); clearTimeout(debounce); clearInterval(timer); });

    return () => h("div", [
      h("h1", "Runs"),
      h("form", { class: "filters", role: "search", onSubmit: event => { event.preventDefault(); clearTimeout(debounce); void go(""); } }, [
        field("Repository", select(filters, "repo", [["", "All repositories"], ...s.repos.map(r => [r, r])], filtered)),
        field("Issue", h("input", { type: "number", min: 1, step: 1, value: filters.issue, onInput: e => { filters.issue = e.target.value; filtered(); } })),
        field("Status", select(filters, "status", [["", "All statuses"], ...statuses.map(v => [v, human(v)])], filtered))
      ]),
      task.state.error ? errorPanel(task.state.error, () => go(s.first)) : null,
      !s.ready && !task.state.error ? loading("Loading runs…") : null,
      s.ready && !s.runs.length ? empty("No runs match these filters.", "Clear a filter, or start a new request.", h("p", h("a", { class: "btn", href: "/runs/new" }, "New request"))) : null,
      s.runs.length ? h("ul", { class: "cards" }, s.runs.map(run => runCard(run))) : null,
      h("div", { class: "actions" }, [
        button("First page", () => go(""), { disabled: task.state.busy || !s.first }),
        button("Older runs", () => go(s.cursor), { disabled: task.state.busy || !s.cursor })
      ])
    ]);
  }
};
