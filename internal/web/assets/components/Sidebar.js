// Sidebar navigation with badge counts, the signed-in identity and the theme
// toggle. On phones it collapses to a top bar with a menu button.
import { button, icon } from "../ui.js";

const { h, reactive } = window.Vue;

const items = [
  { href: "/", page: "inbox", label: "Inbox", icon: "inbox", badge: "inbox" },
  { href: "/runs", page: "runs", label: "Runs", icon: "list", badge: "running" },
  { href: "/runs/new", page: "new", label: "New request", icon: "plus" },
  { href: "/repos", page: "repos", label: "Repositories", icon: "repo" },
  { href: "/memory", page: "memory", label: "Memory", icon: "brain", badge: "memory" },
  { href: "/setup/github", page: "github", label: "GitHub", icon: "github" }
];

export const Sidebar = {
  props: { ctx: { type: Object, required: true } },
  setup(props) {
    const local = reactive({ open: false });
    return () => {
      const { ctx } = props, page = ctx.initial.page;
      const current = page === "detail" ? "runs" : page;
      const dark = ctx.theme.effective() === "dark";
      return h("header", { class: "sidebar" }, [
        h("div", { class: "brand-row" }, [
          h("a", { class: "brand", href: "/" }, "patchbot"),
          h("button", { type: "button", class: "btn quiet menu", "aria-expanded": String(local.open), "aria-controls": "site-nav", "aria-label": "Menu", onClick: () => { local.open = !local.open; } }, [icon("menu", 18)])
        ]),
        h("div", { class: "side-body" + (local.open ? " open" : "") }, [
          h("nav", { id: "site-nav", "aria-label": "Main" }, h("ul", items.map(item => {
            const n = item.badge ? ctx.badges.state[item.badge] : null;
            return h("li", { key: item.page }, h("a", { href: item.href, class: "navlink", "aria-current": item.page === current ? "page" : undefined }, [
              icon(item.icon, 18), h("span", item.label),
              n ? h("span", { class: "count" + (item.badge === "inbox" ? " alert" : ""), "aria-label": `${n} ${item.badge === "inbox" ? "waiting for you" : item.badge === "running" ? "running" : "pending"}` }, String(n)) : null
            ]));
          }))),
          h("div", { class: "side-foot" }, [
            ctx.session.state.email ? h("p", { class: "whoami" }, ["Signed in as ", h("strong", ctx.session.state.email)]) : null,
            h("div", { class: "actions" }, [
              button(dark ? "Light theme" : "Dark theme", ctx.theme.toggle, { icon: dark ? "sun" : "moon", kind: "quiet" }),
              ctx.api.state.authRequired && !ctx.api.state.locked ? button("Lock this tab", ctx.api.forget, { kind: "quiet" }) : null,
              button("Shortcuts", ctx.openHelp, { icon: "keyboard", kind: "quiet" })
            ])
          ])
        ])
      ]);
    };
  }
};
