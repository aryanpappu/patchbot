// Theme toggle. It only sets data-theme on <html>; the choice is a per-viewer
// convenience kept in localStorage, and the page works without it.
const { reactive } = window.Vue;
const key = "patchbot.theme";

export function useTheme() {
  const state = reactive({ theme: "" });
  const root = document.documentElement;
  try {
    const saved = localStorage.getItem(key);
    if (saved === "light" || saved === "dark") state.theme = saved;
  } catch { /* Storage may be blocked; follow the system theme. */ }
  if (state.theme) root.dataset.theme = state.theme;

  const effective = () => state.theme || (window.matchMedia?.("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  function toggle() {
    state.theme = effective() === "dark" ? "light" : "dark";
    root.dataset.theme = state.theme;
    try { localStorage.setItem(key, state.theme); } catch { /* Not persisted. */ }
  }
  return { state, toggle, effective };
}
