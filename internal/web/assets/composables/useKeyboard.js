// Global shortcuts. Ignored while typing, with modifier keys, or while a
// dialog is open. Destructive actions only open the confirm dialog.
export function useKeyboard(bindings, blocked) {
  let pendingG = false, timer = null;

  const typing = target => {
    if (!target || !(target instanceof Element)) return false;
    return target.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName);
  };
  function onKey(event) {
    if (event.ctrlKey || event.metaKey || event.altKey || event.isComposing) return;
    if (typing(event.target) || blocked()) return;
    const key = event.key;
    if (pendingG) {
      pendingG = false; clearTimeout(timer);
      const run = bindings["g " + key];
      if (run) { event.preventDefault(); run(); }
      return;
    }
    if (key === "g") {
      pendingG = true;
      timer = setTimeout(() => { pendingG = false; }, 1200);
      return;
    }
    const run = bindings[key];
    if (run) { event.preventDefault(); run(); }
  }
  return {
    start() { document.addEventListener("keydown", onKey); },
    stop() { document.removeEventListener("keydown", onKey); clearTimeout(timer); }
  };
}
