// Runs one async task at a time and keeps a visible error for it. Failures are
// never swallowed: they land in state.error, rendered as a role="alert" panel.
const { reactive } = window.Vue;

export function useTask() {
  const state = reactive({ busy: false, error: "" });
  async function run(task) {
    state.error = ""; state.busy = true;
    try { return await task(); } catch (error) { state.error = error.message; return undefined; }
    finally { state.busy = false; }
  }
  return { state, run };
}
