// Accessible modal frame: focus moves in on open, Tab is trapped, Escape and a
// backdrop click close it, and focus returns to the opener on close.
const { h, ref, onMounted, onBeforeUnmount } = window.Vue;

const focusable = 'button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

export const Modal = {
  props: { label: { type: String, required: true } },
  emits: ["close"],
  setup(props, { slots, emit }) {
    const root = ref(null);
    let previous = null;
    onMounted(() => {
      previous = document.activeElement;
      const target = root.value.querySelector("[data-autofocus]") || root.value.querySelector(focusable) || root.value;
      target.focus();
    });
    onBeforeUnmount(() => {
      if (previous && document.contains(previous) && previous.focus) previous.focus();
      else document.getElementById("main")?.focus();
    });
    function onKeydown(event) {
      if (event.key === "Escape") { event.preventDefault(); event.stopPropagation(); emit("close"); return; }
      if (event.key !== "Tab") return;
      const items = [...root.value.querySelectorAll(focusable)];
      if (!items.length) { event.preventDefault(); return; }
      const first = items[0], last = items[items.length - 1];
      if (event.shiftKey && (document.activeElement === first || document.activeElement === root.value)) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
    }
    return () => h("div", { class: "backdrop", onClick: event => { if (event.target === event.currentTarget) emit("close"); } },
      h("div", { ref: root, class: "modal", role: "dialog", "aria-modal": "true", "aria-label": props.label, tabindex: -1, onKeydown }, slots.default()));
  }
};
