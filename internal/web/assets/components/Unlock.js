// Token-mode unlock form. The token is kept in sessionStorage by useApi and
// never rendered, put in a URL or kept in reactive state after submit.
import { button, errorPanel, field, input } from "../ui.js";
import { useTask } from "../composables/useTask.js";

const { h, reactive } = window.Vue;

export const Unlock = {
  props: { ctx: { type: Object, required: true }, onUnlocked: Function },
  setup(props) {
    const form = reactive({ token: "" });
    const task = useTask();
    function submit(event) {
      event.preventDefault();
      void task.run(async () => {
        const value = form.token; form.token = "";
        props.ctx.api.unlock(value);
        await props.onUnlocked();
      });
    }
    return () => h("form", { class: "panel narrow", onSubmit: submit }, [
      h("h1", "Unlock patchbot"),
      h("p", "Enter PATCHBOT_UI_TOKEN once for this tab. It is kept only in session storage."),
      task.state.error ? errorPanel(task.state.error) : null,
      field("UI token", input(form, "token", { type: "password", required: true, autocomplete: "off", autofocus: true })),
      button("Unlock", null, { type: "submit", kind: "primary", disabled: task.state.busy })
    ]);
  }
};
