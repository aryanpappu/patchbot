// Tokens, cost and time against the run's frozen limits. Unknown stays unknown.
import { budgetLimits, count, duration, formatMs, money } from "../format.js";
import { meter } from "../ui.js";

const { h } = window.Vue;

export function budgetMeters(run) {
  const limit = budgetLimits(run);
  const sum = [run.input_tokens, run.output_tokens];
  const tokens = sum.some(v => v == null) ? null : sum[0] + sum[1];
  return h("section", { class: "panel", "aria-label": "Budget" }, [
    h("h2", "Budget"),
    h("div", { class: "meters" }, [
      meter("Tokens", tokens, limit.tokens, count),
      meter("Cost", run.cost_micros, limit.cost, money),
      meter("Time", duration(run), limit.ms, formatMs)
    ]),
    h("p", { class: "muted small" }, `Input ${count(run.input_tokens)} · output ${count(run.output_tokens)} · cache read ${count(run.cache_read_tokens)} · cache write ${count(run.cache_write_tokens)}`)
  ]);
}
