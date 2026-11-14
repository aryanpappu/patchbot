// Promise-based confirmation. ConfirmDialog renders `state`; callers await ask().
const { reactive } = window.Vue;

export function useConfirm() {
  const state = reactive({ open: false, title: "", message: "", confirmLabel: "Confirm", danger: false, details: [] });
  let resolve = null;

  function ask(options) {
    if (resolve) resolve(false);
    Object.assign(state, { title: "", message: "", confirmLabel: "Confirm", danger: false, details: [] }, options, { open: true });
    return new Promise(done => { resolve = done; });
  }
  function answer(value) {
    state.open = false;
    const done = resolve;
    resolve = null;
    if (done) done(value);
  }
  return { state, ask, answer };
}
