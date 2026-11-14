// WAI-ARIA tabs with arrow-key, Home and End navigation. Panels are rendered
// by the caller; this only draws the tab list.
const { h } = window.Vue;

export function tabList(label, tabs, current, select) {
  const move = (event, index) => {
    const next = { ArrowRight: index + 1, ArrowLeft: index - 1, Home: 0, End: tabs.length - 1 }[event.key];
    if (next == null) return;
    event.preventDefault();
    const target = tabs[(next + tabs.length) % tabs.length];
    select(target.id);
    document.getElementById("tab-" + target.id)?.focus();
  };
  return h("div", { class: "tabs", role: "tablist", "aria-label": label }, tabs.map((tab, index) => h("button", {
    type: "button", role: "tab", id: "tab-" + tab.id, key: tab.id, class: "tab", "aria-selected": String(tab.id === current),
    "aria-controls": "panel-" + tab.id, tabindex: tab.id === current ? 0 : -1, onClick: () => select(tab.id), onKeydown: event => move(event, index)
  }, tab.label)));
}

export const tabPanel = (id, children) => h("div", { role: "tabpanel", id: "panel-" + id, "aria-labelledby": "tab-" + id, tabindex: 0, class: "tabpanel" }, children);
