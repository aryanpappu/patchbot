// Sidebar badge counts and the `(n) patchbot` tab title. Counts come from the
// existing list API (all pages), never from the current list filter.
const { reactive } = window.Vue;

export function useBadges(api) {
  const state = reactive({ inbox: null, running: null, memory: null });

  async function countStatus(status) {
    let cursor = "", total = 0;
    do {
      const q = new URLSearchParams({ status, limit: "500" });
      if (cursor) q.set("cursor", cursor);
      const page = await api.request("/api/runs?" + q);
      total += page.runs.length;
      cursor = page.next_cursor;
    } while (cursor && !api.state.locked);
    return total;
  }

  function title() {
    document.title = state.inbox ? `(${state.inbox}) patchbot` : "patchbot";
  }

  async function refresh() {
    if (api.state.locked) return;
    try {
      const [waiting, info, running, memory] = await Promise.all([
        countStatus("awaiting_approval"), countStatus("needs_info"), countStatus("running"),
        api.request("/api/memory?status=pending").catch(() => null)
      ]);
      state.inbox = waiting + info;
      state.running = running;
      state.memory = memory ? (memory.entries || []).length : null;
    } catch { /* Badges are best effort; the page shows its own errors. */ }
    title();
  }
  function clear() { state.inbox = state.running = state.memory = null; title(); }
  return { state, refresh, clear };
}
