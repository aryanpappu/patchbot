// The Live log tab: events grouped by turn/phase, tool output collapsed behind
// details, failing events marked, and a follow toggle that keeps the newest
// event in view. Every event payload is rendered as text.
import { groupEvents } from "../format.js";
import { block, button, empty, icon } from "../ui.js";

const { h, reactive, onUpdated, ref } = window.Vue;

export const LogStream = {
  props: { stream: { type: Object, required: true } },
  setup(props) {
    const local = reactive({ follow: true });
    const box = ref(null);
    onUpdated(() => { if (local.follow && box.value) box.value.scrollTop = box.value.scrollHeight; });
    return () => {
      const st = props.stream.state;
      const groups = groupEvents(st.events);
      return h("section", { class: "panel", "aria-label": "Live log" }, [
        h("div", { class: "log-head" }, [
          h("h2", "Live log"),
          button(local.follow ? "Following latest" : "Follow latest", () => { local.follow = !local.follow; }, { pressed: local.follow, icon: "dots" })
        ]),
        h("p", { class: "muted small", role: "status" }, `${st.status} · last event ${st.lastID} · showing the latest 1,000 events`),
        groups.length ? h("div", { class: "events", ref: box, tabindex: 0, role: "region", "aria-label": "Run event log" }, groups.map((group, gi) =>
          h("section", { key: gi + group.label, class: "event-group" }, [
            h("h3", group.label),
            h("ol", group.items.map(event => h("li", { key: event.id, class: event.failing ? "failing" : "" }, [
              h("details", [
                h("summary", [event.failing ? icon("x-circle", 14) : null, h("span", { class: "mono" }, `${event.id} · ${event.type}`), event.failing ? h("strong", " failing") : null]),
                block(event.output, "Event " + event.id)
              ])
            ])))
          ]))) : empty("No events yet.", "Events appear here as the run works.")
      ]);
    };
  }
};
