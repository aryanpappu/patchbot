// The Changes tab: changed files and diff as text, test result and PR link.
import { human } from "../format.js";
import { block, chip, empty, link, panel } from "../ui.js";
import { DiffView } from "./DiffView.js";

const { h } = window.Vue;

export function changesView(d, artifacts) {
  const run = d.run;
  const diff = artifacts.find(a => a.kind === "diff");
  const tests = artifacts.filter(a => ["test_output", "baseline"].includes(a.kind));
  const reviewed = (d.reviews && d.reviews.length) || run.phase === "review";
  return h("div", [
    panel("Pull request", [
      run.prurl ? h("p", link(run.prurl, "Open the pull request")) : empty("No pull request yet.", run.dry_run ? "This is a dry run; nothing is published." : "It is opened after the code passes tests and review."),
      ...artifacts.filter(a => ["pr_title", "pr_body"].includes(a.kind)).map(a => h("div", { key: a.kind }, [h("h3", human(a.kind)), block(a.content, human(a.kind))]))
    ]),
    panel("Diff", [diff ? h(DiffView, { diff: diff.content }) : empty("No diff recorded yet.", "The diff appears after the code step.")]),
    panel("Test result", [
      h("p", `Harness: ${run.tests_status || "not recorded"} · ${run.test_attempts} attempt${run.test_attempts === 1 ? "" : "s"}`),
      ...tests.map(a => h("details", { key: a.kind + a.attempt, open: a.kind === "test_output" && run.tests_status === "failed" },
        [h("summary", `${human(a.kind)} · attempt ${a.attempt}`), block(a.content, human(a.kind))]))
    ]),
    reviewed ? panel("Independent review", [
      h("p", { class: "muted" }, "Advisory reviewer verdicts, each bound to the approved spec hash and the reviewed diff hash. A review never replaces approval or harness tests."),
      ...(d.reviews || []).map(r => h("p", { key: r.id }, [chip(r.verdict), ` round ${r.round} · ${r.provider} / ${r.model}${r.reason ? " · " + r.reason : ""} · diff ${String(r.diff_hash).slice(0, 12)}`])),
      ...artifacts.filter(a => a.kind === "review").map(a => h("details", { key: "r" + a.attempt }, [h("summary", `Review round ${a.attempt}`), block(a.content, "Review")]))
    ]) : null
  ]);
}
