// Refine > Approval > Code > Tests > Review > PR, with the failure reason in
// plain language and what to do next. State comes only from the API's status.
import { failureInfo, stepperState } from "../format.js";
import { icon } from "../ui.js";

const { h } = window.Vue;
const marks = { done: "check", current: "dots", failed: "x-circle", stopped: "stop", todo: "" };
const words = { done: "done", current: "in progress", failed: "failed", stopped: "stopped", todo: "not reached" };

export function phaseStepper(run) {
  const failure = failureInfo(run);
  return h("section", { class: "stepper-wrap", "aria-label": "Progress" }, [
    h("ol", { class: "stepper" }, stepperState(run).map(step => h("li", { class: "step " + step.state, key: step.label, "aria-current": step.state === "current" ? "step" : undefined }, [
      h("span", { class: "dot" }, step.state === "todo" ? null : icon(marks[step.state], 14)),
      h("span", { class: "step-label" }, step.label),
      h("span", { class: "sr-only" }, ", " + words[step.state])
    ]))),
    failure ? h("div", { class: "alert", role: "alert" }, [icon("warn", 16), h("div", [
      h("p", h("strong", failure.title)),
      h("p", ["What to do next: ", failure.next]),
      failure.detail ? h("p", { class: "mono small" }, failure.detail) : null
    ])]) : null
  ]);
}
