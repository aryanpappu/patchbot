// Provider and model pickers backed by useProviders (live listing + typing).
import { checkbox, field, input, select } from "../ui.js";

const { h } = window.Vue;

export function modelFields(providers, refine = false) {
  const { state, form } = providers;
  return [
    field("Provider", select(form, "provider", state.providers.map(p => [p.id, p.id + (p.available ? "" : " (unavailable)")]), () => providers.loadModels(true))),
    field("Model (choose or type an ID)", input(form, "model", { list: "model-ids", required: true, maxlength: 512 })),
    h("datalist", { id: "model-ids" }, state.models.map(m => h("option", { value: m.id }, m.display_name || m.id))),
    state.warning ? h("p", { class: "warning", role: "status" }, state.warning) : null,
    refine ? checkbox(form, "separate", "Use a separate refine model") : null,
    refine && form.separate ? field("Refine model (same provider)", input(form, "refine", { list: "model-ids", required: true, maxlength: 512 })) : null
  ];
}
