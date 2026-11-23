// Open questions from refinement, one answer box each. Submitting records the
// answers and asks for a new specification version that uses them. Earlier
// answers are listed read-only. The server decides whether answering is valid.
import { decodeSpec } from "../format.js";
import { button, field } from "../ui.js";

const { h, reactive, watch } = window.Vue;

export const QuestionsPanel = {
  props: { d: Object, busy: Boolean, onSubmit: Function },
  setup(props) {
    const s = reactive({ answers: [], key: "" });
    // Reset the boxes whenever a different spec version is shown.
    watch(() => props.d?.latest_spec?.hash, hash => {
      const questions = decodeSpec(props.d?.latest_spec)?.open_questions || [];
      if (hash !== s.key) { s.key = hash; s.answers = questions.map(() => ""); }
    }, { immediate: true });

    return () => {
      const d = props.d, spec = d?.latest_spec;
      const questions = decodeSpec(spec)?.open_questions || [];
      const answered = (d?.questions || []).filter(q => q.answer);
      const canAnswer = !!d?.capabilities?.can_answer && questions.length > 0;
      if (!canAnswer && answered.length === 0) return null;
      const ready = s.answers.length === questions.length && s.answers.every(a => a.trim());
      return h("section", { class: "card questions", "aria-label": "Questions from patchbot" }, [
        h("h2", canAnswer ? `patchbot needs ${questions.length === 1 ? "an answer" : questions.length + " answers"}` : "Answered questions"),
        canAnswer ? h("p", { class: "meta" }, "Answer each question. patchbot then writes a new specification version that uses your answers.") : null,
        canAnswer ? h("form", { onSubmit: event => { event.preventDefault(); if (ready) props.onSubmit(s.answers.slice(), spec); } }, [
          ...questions.map((q, i) => field(`${i + 1}. ${q}`, h("textarea", {
            rows: 2, required: true, maxlength: 4000, value: s.answers[i],
            onInput: event => { s.answers[i] = event.target.value; }
          }))),
          h("div", { class: "actions" }, [button("Send answers", null, { type: "submit", kind: "primary", icon: "check", disabled: props.busy || !ready })])
        ]) : null,
        answered.length ? h("details", { open: !canAnswer }, [
          h("summary", `Earlier answers (${answered.length})`),
          h("dl", answered.map(q => [h("dt", q.body), h("dd", `${q.answer} — ${q.answered_by || "unknown"}`)]).flat())
        ]) : null
      ]);
    };
  }
};
