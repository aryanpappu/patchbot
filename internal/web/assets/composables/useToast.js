// Toasts: role="status" messages that dismiss themselves after 6 seconds.
const { reactive } = window.Vue;

export function useToast() {
  const state = reactive({ items: [] });
  const timers = new Map();
  let next = 1;

  function dismiss(id) {
    clearTimeout(timers.get(id));
    timers.delete(id);
    const index = state.items.findIndex(item => item.id === id);
    if (index >= 0) state.items.splice(index, 1);
  }
  function push(message, kind = "ok") {
    const id = next++;
    state.items.push({ id, message: String(message), kind });
    if (state.items.length > 4) dismiss(state.items[0].id);
    timers.set(id, setTimeout(() => dismiss(id), 6000));
  }
  function dispose() { for (const timer of timers.values()) clearTimeout(timer); timers.clear(); }
  return { state, push, dismiss, dispose };
}
