// Pure helpers: no DOM access, no Vue. Everything here returns plain strings,
// numbers or plain objects so it can be unit-tested and so untrusted text is
// only ever handed to the renderer as text.

export const active = ["queued", "running", "awaiting_approval", "needs_info", "approved"];
export const statuses = [...active, "succeeded", "failed", "cancelled", "rejected"];

// One mapping for every status chip: icon + text + colour class.
export const statusMeta = {
  awaiting_approval: { icon: "clock", label: "Awaiting approval" },
  needs_info: { icon: "question", label: "Needs info" },
  queued: { icon: "dots", label: "Queued" },
  running: { icon: "spinner", label: "Running" },
  approved: { icon: "check", label: "Approved" },
  succeeded: { icon: "check-circle", label: "Succeeded" },
  failed: { icon: "x-circle", label: "Failed" },
  rejected: { icon: "ban", label: "Rejected" },
  cancelled: { icon: "stop", label: "Cancelled" }
};

export const text = value => value == null ? "unknown" : String(value);
export const money = value => value == null ? "unknown" : `$${(value / 1000000).toFixed(4)}`;
export const human = value => String(value || "pending").replaceAll("_", " ");
export const lines = value => String(value || "").split("\n").map(line => line.trim()).filter(Boolean);
export const shortHash = hash => String(hash || "").slice(0, 9);
export const count = value => value == null ? "unknown" : Number(value).toLocaleString("en-US");

// Escape every rune a lesson may not contain (format, bidi, tag, private-use,
// separators, controls other than newline/tab) so nothing hides from review.
const printable = /[\p{L}\p{M}\p{N}\p{P}\p{S} \n\t]/u;
export const visible = value => Array.from(String(value ?? ""), c => printable.test(c) ? c
  : String.fromCharCode(92) + "u{" + c.codePointAt(0).toString(16).toUpperCase() + "}").join("");

// linkHref returns a normalised http(s) URL or "" for anything else.
export function linkHref(url) {
  try {
    const parsed = new URL(url);
    if (["http:", "https:"].includes(parsed.protocol)) return parsed.href;
  } catch { /* An invalid external link stays text. */ }
  return "";
}

export const specText = spec => (spec?.body_md || "").replace(/\n<!-- patchbot:spec-data [A-Za-z0-9+/=]+ -->\n$/, "");

// decodeSpec recovers the structured fields hidden in the spec marker, or null.
export function decodeSpec(spec) {
  const match = (spec?.body_md || "").match(/\n<!-- patchbot:spec-data ([A-Za-z0-9+/=]+) -->\n$/);
  if (!match) return null;
  try {
    const data = JSON.parse(new TextDecoder().decode(Uint8Array.from(atob(match[1]), c => c.charCodeAt(0))));
    const list = value => Array.isArray(value) ? value.map(String) : [];
    return {
      summary: String(data.summary || ""), acceptance_criteria: list(data.acceptance_criteria), risks: list(data.risks),
      test_plan: list(data.test_plan), open_questions: list(data.open_questions),
      files: Array.isArray(data.files) ? data.files.map(f => ({ path: String(f?.path || ""), change: String(f?.change || "") })) : []
    };
  } catch { return null; }
}

// A linear replacement diff: common prefix/suffix stay as context; changed
// lines are shown in full. No quadratic work on model-generated documents.
export function lineDiff(before, after) {
  const a = before.split("\n"), b = after.split("\n");
  let start = 0, end = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) start++;
  while (end < a.length - start && end < b.length - start && a[a.length - end - 1] === b[b.length - end - 1]) end++;
  if (start === a.length && start === b.length) return "No changes.";
  return [
    ...a.slice(Math.max(0, start - 3), start).map(line => "  " + line),
    ...a.slice(start, a.length - end).map(line => "- " + line),
    ...b.slice(start, b.length - end).map(line => "+ " + line),
    ...b.slice(b.length - end, b.length - end + 3).map(line => "  " + line)
  ].join("\n");
}

// parseDiff splits a unified diff into files with typed lines, in linear time.
// Unrecognised input still yields one "(diff)" file so nothing is dropped.
export function parseDiff(raw) {
  const files = [];
  let file = null;
  const open = path => { file = { path, adds: 0, dels: 0, lines: [] }; files.push(file); };
  for (const line of String(raw || "").split("\n")) {
    if (line.startsWith("diff --git ")) {
      open(line.replace(/^diff --git a\/(.*) b\/.*$/, "$1"));
      file.lines.push({ kind: "meta", text: line });
      continue;
    }
    if (!file) open("(diff)");
    let kind = "ctx";
    if (/^(\+\+\+|---|index |new file|deleted file|similarity|rename |old mode|new mode|Binary)/.test(line)) kind = "meta";
    else if (line.startsWith("@@")) kind = "hunk";
    else if (line.startsWith("+")) { kind = "add"; file.adds++; }
    else if (line.startsWith("-")) { kind = "del"; file.dels++; }
    file.lines.push({ kind, text: line });
  }
  return files;
}

// Phase stepper: Refine, Approval, Code, Tests, Review, PR. The state of each
// step comes only from the run status/phase the API reports.
export const stepperPhases = ["Refine", "Approval", "Code", "Tests", "Review", "PR"];
const phaseToStep = { preflight: 0, refine: 0, clone: 2, baseline: 3, code: 2, verify: 3, review: 4, publish: 5 };

export function stepperState(run) {
  const waiting = run.status === "awaiting_approval" || run.status === "needs_info";
  let current = waiting ? 1 : (phaseToStep[run.phase] ?? 0);
  if (run.status === "approved") current = 1;
  if (run.status === "queued") current = 0;
  const done = run.status === "succeeded";
  const failed = ["failed", "rejected", "cancelled"].includes(run.status);
  return stepperPhases.map((label, index) => {
    let state = "todo";
    if (done || index < current) state = "done";
    else if (index === current) state = failed ? (run.status === "failed" ? "failed" : "stopped") : "current";
    return { label, state };
  });
}

const failureText = {
  expired: ["The approval wait expired.", "Retry the run to produce a fresh specification."],
  interrupted: ["patchbot restarted while this run was working.", "Retry the run; nothing was published."],
  budget_exhausted: ["The run used up its cost budget.", "Retry with a larger budget."],
  review_failed: ["The independent reviewer asked for changes.", "Read the review in the Changes tab, then revise or retry."],
  review_stale: ["The code changed after it was reviewed.", "Retry the run so the review matches the final diff."],
  provider_auth: ["The model provider rejected the API key.", "Fix the provider key in the server config, then retry."],
  rate_limit: ["The model provider is rate limiting requests.", "Wait a few minutes and retry."],
  tests_failed: ["The tests did not pass.", "Open the Changes tab to read the test output, then revise or retry."]
};

// failureInfo explains a failed/stopped run in plain language, or returns null.
export function failureInfo(run) {
  if (run.status === "rejected") return { title: "You rejected this specification.", next: "Retry to start over with a new specification.", detail: "" };
  if (run.status === "cancelled") return { title: "This run was cancelled.", next: "Retry to start a new run.", detail: "" };
  if (run.status !== "failed" && !run.error_kind) return null;
  const known = failureText[run.error_kind];
  return {
    title: known ? known[0] : "The run stopped with an error.",
    next: known ? known[1] : "Read the details below, then retry or revise.",
    detail: [run.error_kind, run.error_message].filter(Boolean).join(": ")
  };
}

export function duration(run, now = Date.now()) {
  if (!run.started_at) return null;
  const end = run.finished_at ? Date.parse(run.finished_at) : now;
  const ms = end - Date.parse(run.started_at);
  return Number.isFinite(ms) && ms >= 0 ? ms : null;
}

export function formatMs(ms) {
  if (ms == null) return "unknown";
  const total = Math.round(ms / 1000);
  const minutes = Math.floor(total / 60);
  return minutes ? `${minutes}m ${total % 60}s` : `${total}s`;
}

// budgetLimits reads the frozen per-run limits (Go field names, no JSON tags);
// anything missing or non-positive is null, shown as "unknown".
export function budgetLimits(run) {
  const l = run.limits_json && typeof run.limits_json === "object" ? run.limits_json : {};
  const a = l.Agent && typeof l.Agent === "object" ? l.Agent : {};
  const pos = value => Number.isFinite(value) && value > 0 ? value : null;
  return { cost: pos(a.MaxCostMicros), tokens: pos(a.MaxTotalTokens), ms: pos(l.Timeout) == null ? null : l.Timeout / 1e6 };
}

// eventInfo labels a log event by turn, else phase, else type. Payloads are
// untrusted: only plain scalar fields are read.
export function eventInfo(event) {
  let payload = null;
  try { payload = JSON.parse(event.output); } catch { /* Malformed data is shown as text. */ }
  const p = payload && typeof payload === "object" ? payload : {};
  const turn = Number.isSafeInteger(p.turn) ? p.turn : null;
  const phase = typeof p.phase === "string" ? p.phase : (event.type === "run.status" && typeof p.status === "string" ? p.status : "");
  const failing = p.ok === false || ["failed", "error"].includes(p.status) || (Number.isFinite(p.exit_code) && p.exit_code !== 0) || /\b(FAIL|panic:)\b/.test(event.output);
  return { group: turn != null ? `Turn ${turn}` : phase ? human(phase) : "Events", failing };
}

export function groupEvents(events) {
  const groups = [];
  for (const event of events) {
    const info = eventInfo(event);
    let group = groups[groups.length - 1];
    if (!group || group.label !== info.group) { group = { label: info.group, items: [] }; groups.push(group); }
    group.items.push({ ...event, failing: info.failing });
  }
  return groups;
}
