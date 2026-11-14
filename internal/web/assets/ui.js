// UI primitives. Every untrusted value passes through as a Vue text child;
// there is no HTML string anywhere. Reuse these instead of re-creating them.
import { human, linkHref, statusMeta, text } from "./format.js";

const { h } = window.Vue;

// Inline SVG icons, 24x24 stroke paths. Decorative: the label is always text.
const paths = {
  clock: ["M12 7v5l3 2", "M12 3a9 9 0 1 0 0 18a9 9 0 0 0 0-18z"],
  question: ["M9.5 9a2.5 2.5 0 1 1 3.5 2.3c-.7.4-1 .9-1 1.7", "M12 17h.01", "M12 3a9 9 0 1 0 0 18a9 9 0 0 0 0-18z"],
  dots: ["M7 12h.01", "M12 12h.01", "M17 12h.01"],
  spinner: ["M12 3a9 9 0 0 1 9 9", "M12 21a9 9 0 0 1-9-9"],
  check: ["M5 12l5 5L20 7"],
  "check-circle": ["M8 12l3 3 5-6", "M12 3a9 9 0 1 0 0 18a9 9 0 0 0 0-18z"],
  "x-circle": ["M9 9l6 6", "M15 9l-6 6", "M12 3a9 9 0 1 0 0 18a9 9 0 0 0 0-18z"],
  ban: ["M12 3a9 9 0 1 0 0 18a9 9 0 0 0 0-18z", "M5.6 5.6l12.8 12.8"],
  stop: ["M6 6h12v12H6z"],
  inbox: ["M3 13l3-8h12l3 8", "M3 13v6h18v-6", "M3 13h5l1 3h6l1-3h5"],
  list: ["M8 6h12", "M8 12h12", "M8 18h12", "M4 6h.01", "M4 12h.01", "M4 18h.01"],
  plus: ["M12 5v14", "M5 12h14"],
  repo: ["M5 4h12a2 2 0 0 1 2 2v14H7a2 2 0 0 1-2-2z", "M5 18a2 2 0 0 1 2-2h12"],
  brain: ["M9 4a3 3 0 0 0-3 3v1a3 3 0 0 0 0 6v1a3 3 0 0 0 6 0V7a3 3 0 0 0-3-3z", "M15 4a3 3 0 0 1 3 3v1a3 3 0 0 1 0 6v1a3 3 0 0 1-6 0"],
  github: ["M9 19c-4 1.5-4-2-6-2", "M15 22v-3.5a3 3 0 0 0-1-2.5c3 0 6-1.5 6-5.5a4.5 4.5 0 0 0-1.2-3 4 4 0 0 0 0-3s-1 0-3 1.3a10 10 0 0 0-5.6 0C8 4 7 4 7 4a4 4 0 0 0 0 3 4.5 4.5 0 0 0-1.2 3c0 4 3 5.5 6 5.5a3 3 0 0 0-1 2.5V22"],
  sun: ["M12 8a4 4 0 1 0 0 8a4 4 0 0 0 0-8z", "M12 2v2", "M12 20v2", "M2 12h2", "M20 12h2", "M5 5l1.5 1.5", "M17.5 17.5L19 19", "M5 19l1.5-1.5", "M17.5 6.5L19 5"],
  moon: ["M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z"],
  menu: ["M4 6h16", "M4 12h16", "M4 18h16"],
  external: ["M14 4h6v6", "M20 4l-9 9", "M18 14v5H5V6h5"],
  warn: ["M12 3l10 18H2z", "M12 10v4", "M12 17h.01"],
  keyboard: ["M3 6h18v12H3z", "M7 10h.01", "M11 10h.01", "M15 10h.01", "M8 14h8"]
};

export function icon(name, size = 16) {
  return h("svg", {
    class: "icon", width: size, height: size, viewBox: "0 0 24 24", fill: "none", stroke: "currentColor",
    "stroke-width": 2, "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true", focusable: "false"
  }, (paths[name] || paths.dots).map(d => h("path", { d })));
}

export const chip = status => {
  const meta = statusMeta[status];
  return h("span", { class: "chip s-" + (meta ? status : "unknown") }, [icon(meta ? meta.icon : "dots", 14), meta ? meta.label : human(status)]);
};

export function button(label, click, options = {}) {
  const { disabled = false, kind = "", icon: iconName = "", type = "button", title, ariaLabel, pressed } = options;
  return h("button", {
    type, class: "btn" + (kind ? " " + kind : ""), onClick: click, disabled, title, "aria-label": ariaLabel,
    "aria-pressed": pressed == null ? undefined : String(pressed)
  }, [iconName ? icon(iconName, 16) : null, label]);
}

export const field = (label, control, hint) => h("label", { class: "field" }, [h("span", label), control, hint ? h("small", hint) : null]);
export const input = (object, key, attributes = {}) => h("input", {
  ...attributes, value: object[key], onInput: event => { object[key] = event.target.value; }
});
export const area = (object, key, attributes = {}) => h("textarea", {
  rows: 4, ...attributes, value: object[key], onInput: event => { object[key] = event.target.value; }
});
export const select = (object, key, options, change) => h("select", {
  value: object[key], onChange: event => { object[key] = event.target.value; if (change) change(); }
}, options.map(([value, label]) => h("option", { value, selected: value === object[key] }, label)));
export const checkbox = (object, key, label) => h("label", { class: "check" }, [
  h("input", { type: "checkbox", checked: !!object[key], onChange: event => { object[key] = event.target.checked; } }), label]);

export const panel = (title, children, attributes = {}) => h("section", { class: "panel", ...attributes }, [title ? h("h2", title) : null, ...children]);

// block renders untrusted text in a keyboard-scrollable monospace panel.
export const block = (value, label) => h("pre", { class: "output", tabindex: 0, "aria-label": label }, text(value));

// link makes an anchor only for validated http(s) URLs; anything else is text.
export const link = (url, label) => {
  const href = linkHref(url);
  return href ? h("a", { href, target: "_blank", rel: "noopener noreferrer" }, [label, icon("external", 12)]) : h("span", label);
};

export const loading = (label = "Loading…") => h("p", { class: "state", role: "status" }, [icon("spinner", 16), label]);
export const empty = (title, next, action) => h("div", { class: "state empty" }, [h("p", h("strong", title)), next ? h("p", { class: "muted" }, next) : null, action || null]);
export const errorPanel = (message, retry) => h("div", { class: "alert", role: "alert" }, [
  icon("warn", 16), h("div", [h("p", message), retry ? button("Try again", retry) : null])]);

// meter shows used/limit as text and a bar; unknown values stay "unknown".
export function meter(label, used, limit, show) {
  const known = used != null && limit != null;
  const pct = known ? Math.min(100, Math.round(used / limit * 100)) : 0;
  return h("div", { class: "meter" }, [
    h("div", { class: "meter-head" }, [h("span", label), h("strong", show(used) + (limit != null ? " of " + show(limit) : " (limit unknown)"))]),
    known ? h("progress", { class: pct >= 90 ? "hot" : "", max: 100, value: pct, "aria-label": label + " used" }, pct + "%") : null
  ]);
}
