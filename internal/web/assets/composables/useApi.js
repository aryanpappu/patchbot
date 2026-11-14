// The single fetch helper. In token mode the bearer token lives only in
// sessionStorage and a closure; in Access mode there is no credential at all.
const { reactive } = window.Vue;
const storageKey = "patchbot.ui-token";

export function useApi(authRequired) {
  let token = "";
  try { token = sessionStorage.getItem(storageKey) || ""; } catch { /* The unlock form explains storage failures. */ }
  const state = reactive({ authRequired, locked: authRequired && !token });
  const listeners = [];

  const headers = (extra = {}) => token ? { ...extra, Authorization: "Bearer " + token } : extra;

  function forget() {
    token = "";
    try { sessionStorage.removeItem(storageKey); } catch { /* No persistent fallback. */ }
    state.locked = true;
    for (const listener of listeners) listener();
  }

  // unlock stores a token for this tab only. It throws when storage is blocked.
  function unlock(value) {
    const candidate = value.trim();
    if (!candidate) throw new Error("Enter the UI token.");
    try { sessionStorage.setItem(storageKey, candidate); }
    catch { throw new Error("Enable session storage to use token authentication in this tab."); }
    token = candidate;
    state.locked = false;
  }

  async function request(path, options = {}, plain = false) {
    const response = await fetch(path, { ...options, credentials: "same-origin", headers: headers(options.headers) });
    if (response.status === 401) {
      if (state.authRequired) forget();
      throw new Error(state.authRequired ? "Enter the UI token to continue." : "Sign in through Cloudflare Access to continue.");
    }
    if (!response.ok) {
      let message = `Request failed (${response.status}).`;
      let failure = null;
      try { failure = await response.json(); message = failure.error?.message || message; } catch { /* Use the status if the body is not JSON. */ }
      throw Object.assign(new Error(message), { body: failure, status: response.status });
    }
    return plain ? response.text() : response.json();
  }

  const send = (path, body, method = "POST") => request(path, {
    method, headers: { "Content-Type": "application/json" }, body: JSON.stringify(body)
  });

  return { state, request, send, headers, forget, unlock, onForget: listener => { listeners.push(listener); } };
}
