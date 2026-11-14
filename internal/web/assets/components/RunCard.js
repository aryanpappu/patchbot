// One run as a card: used by the inbox and the run list.
import { human, money, runHref } from "../format.js";
import { chip, icon, link } from "../ui.js";

const { h } = window.Vue;

export function ago(iso, now = Date.now()) {
  const ms = now - Date.parse(iso || "");
  if (!Number.isFinite(ms) || ms < 0) return "";
  const minutes = Math.floor(ms / 60000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.floor(minutes / 60);
  return hours < 48 ? `${hours} h ago` : `${Math.floor(hours / 24)} days ago`;
}

export function runCard(run, hint) {
  const waited = run.waiting_since ? ago(run.waiting_since) : "";
  return h("li", { key: run.id, class: "card" }, [
    h("div", { class: "card-head" }, [
      h("h3", h("a", { href: runHref(run.id) }, `${run.repo} #${run.issue_number}`)),
      chip(run.status)
    ]),
    h("p", { class: "card-title" }, run.issue_title || "(untitled)"),
    h("p", { class: "meta" }, [
      `run ${run.id} · ${human(run.phase)} · cost ${money(run.cost_micros)}${run.dry_run ? " · dry run" : ""}`,
      waited ? ` · waiting ${waited}` : ""
    ]),
    hint ? h("p", { class: "hint" }, [icon("clock", 14), hint]) : null,
    run.prurl ? h("p", link(run.prurl, "Pull request")) : null
  ]);
}
