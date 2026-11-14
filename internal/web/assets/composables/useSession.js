// Signed-in identity. Access mode: GET /api/session returns the verified email.
// Token mode: no identity; the email stays empty.
const { reactive } = window.Vue;

export function useSession(api, email) {
  const state = reactive({ email: email || "", mode: "", loaded: false });
  async function load() {
    try {
      const body = await api.request("/api/session");
      state.email = typeof body.email === "string" ? body.email : "";
      state.mode = typeof body.mode === "string" ? body.mode : "";
    } catch { /* The shell already carries the email; a failure here is not fatal. */ }
    state.loaded = true;
  }
  return { state, load };
}
