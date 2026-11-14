// Providers, repositories and model lists shared by the request and retry forms.
const { reactive } = window.Vue;

export function useProviders(api) {
  const state = reactive({ providers: [], repos: [], models: [], warning: "", defaults: null, loaded: false });
  const form = reactive({ repo: "", issue: "", mode: "fix", budget: "2", dry: true, provider: "", model: "", separate: false, refine: "" });
  let serial = 0;

  async function loadModels(reset = false) {
    const mine = ++serial;
    state.models = []; state.warning = "Loading models…";
    if (reset) {
      form.model = state.providers.find(p => p.id === form.provider)?.default_model || "";
      form.refine = ""; form.separate = false;
    }
    try {
      const list = await api.request(`/api/providers/${encodeURIComponent(form.provider)}/models`);
      if (mine !== serial) return;
      state.models = list.models || [];
      state.warning = list.warning || (!list.live ? "Live listing failed. Choose a configured model or type a model ID." : "");
    } catch (error) {
      if (mine === serial) state.warning = error.message + " Type a model ID to continue.";
    }
  }

  async function load() {
    const body = await api.request("/api/providers");
    state.providers = body.providers; state.repos = body.repos; state.defaults = body.defaults;
    form.repo = state.repos[0] || "";
    form.provider = body.defaults.provider;
    form.mode = body.defaults.mode;
    form.model = body.defaults.code_model || "";
    form.refine = body.defaults.refine_model || "";
    form.separate = !!form.refine && form.refine !== form.model;
    form.budget = String(body.defaults.max_cost_usd);
    state.loaded = true;
  }
  const reset = () => { serial++; state.providers = []; state.repos = []; state.models = []; state.loaded = false; };
  return { state, form, load, loadModels, reset };
}
