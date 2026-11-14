// Application frame: sidebar, routed page, toasts and dialogs. Pages mount only
// while the tab is unlocked, so locking tears down their timers and streams.
import { errorPanel } from "../ui.js";
import { useKeyboard } from "../composables/useKeyboard.js";
import { ConfirmDialog } from "./ConfirmDialog.js";
import { HelpDialog } from "./HelpDialog.js";
import { Inbox } from "./Inbox.js";
import { MemoryView } from "./MemoryView.js";
import { NewRequest } from "./NewRequest.js";
import { RepoPage } from "./RepoPage.js";
import { RunDetail } from "./RunDetail.js";
import { RunList } from "./RunList.js";
import { Sidebar } from "./Sidebar.js";
import { Toast } from "./Toast.js";
import { Unlock } from "./Unlock.js";

const { h, reactive, onMounted, onUnmounted } = window.Vue;
const pages = { inbox: Inbox, runs: RunList, new: NewRequest, repos: RepoPage, memory: MemoryView, detail: RunDetail };

export const App = {
  props: { ctx: { type: Object, required: true } },
  setup(props) {
    const { ctx } = props, { api } = ctx;
    const local = reactive({ help: false, error: "" });
    let timer = null;
    ctx.openHelp = () => { local.help = true; };
    Object.assign(ctx.shortcuts, {
      "g i": () => location.assign("/"), "g r": () => location.assign("/runs"), "g n": () => location.assign("/runs/new"), "?": ctx.openHelp
    });
    const keyboard = useKeyboard(ctx.shortcuts, () => ctx.confirm.state.open || local.help);

    async function boot() {
      await ctx.session.load();
      await ctx.badges.refresh();
    }
    api.onForget(() => { ctx.badges.clear(); ctx.providers.reset(); });
    onMounted(() => {
      keyboard.start();
      if (!api.state.locked) void boot();
      timer = setInterval(() => { if (!api.state.locked) void ctx.badges.refresh(); }, 15000);
    });
    onUnmounted(() => { keyboard.stop(); clearInterval(timer); ctx.toast.dispose(); });

    return () => {
      const Page = pages[ctx.initial.page] || Inbox;
      return h("div", { class: "frame" }, [
        h(Sidebar, { ctx }),
        h("main", { id: "main", tabindex: -1 }, [
          local.error ? errorPanel(local.error) : null,
          api.state.locked ? h(Unlock, { ctx, onUnlocked: boot }) : h(Page, { ctx, key: ctx.initial.page + ctx.initial.run_id })
        ]),
        h(Toast, { toast: ctx.toast }),
        h(ConfirmDialog, { confirm: ctx.confirm }),
        h(HelpDialog, { open: local.help, onClose: () => { local.help = false; } })
      ]);
    };
  }
};
