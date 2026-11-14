// Landing page: "Waiting for you" lists specs to approve and questions to
// answer, then a short list of runs in progress.
import { empty, errorPanel, icon, loading } from "../ui.js";
import { useTask } from "../composables/useTask.js";
import { runCard } from "./RunCard.js";

const { h, reactive, onMounted, onUnmounted } = window.Vue;

export const Inbox = {
  props: { ctx: { type: Object, required: true } },
  setup(props) {
    const { api } = props.ctx;
    const s = reactive({ ready: false, approve: [], answer: [], running: [] });
    const task = useTask();
    let timer = null, disposed = false;

    async function page(status, limit) {
      return (await api.request("/api/runs?" + new URLSearchParams({ status, limit: String(limit) }))).runs;
    }
    const load = () => task.run(async () => {
      const [approve, answer, running] = await Promise.all([page("awaiting_approval", 100), page("needs_info", 100), page("running", 10)]);
      if (disposed) return;
      Object.assign(s, { approve, answer, running, ready: true });
      void props.ctx.badges.refresh();
    });
    onMounted(() => { void load(); timer = setInterval(() => { if (!task.state.busy && !api.state.locked) void load(); }, 15000); });
    onUnmounted(() => { disposed = true; clearInterval(timer); });

    return () => {
      const list = (runs, hint) => h("ul", { class: "cards" }, runs.map(run => runCard(run, hint)));
      return h("div", [
        h("h1", "Waiting for you"),
        task.state.error ? errorPanel(task.state.error, load) : null,
        !s.ready && !task.state.error ? loading("Loading your inbox…") : null,
        s.ready && !s.approve.length && !s.answer.length ? empty("Nothing is waiting for you.", "New specifications and questions appear here.",
          h("p", h("a", { class: "btn primary", href: "/runs/new" }, [icon("plus", 16), "Write a new request"]))) : null,
        s.approve.length ? h("section", { "aria-labelledby": "h-approve" }, [h("h2", { id: "h-approve" }, `Specifications to approve (${s.approve.length})`),
          list(s.approve, "Read the spec, then approve, revise or reject.")]) : null,
        s.answer.length ? h("section", { "aria-labelledby": "h-answer" }, [h("h2", { id: "h-answer" }, `Questions to answer (${s.answer.length})`),
          list(s.answer, "patchbot needs more information. Revise the spec with your answers.")]) : null,
        s.ready && s.running.length ? h("section", { "aria-labelledby": "h-run" }, [h("h2", { id: "h-run" }, "In progress"), list(s.running)]) : null,
        s.ready ? h("p", h("a", { href: "/runs" }, "See all runs")) : null
      ]);
    };
  }
};
