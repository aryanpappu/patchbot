// Entry point, loaded as <script type="module"> from this origin after the
// pinned Vue global build. Shared services are created once here and handed to
// components with provide-free props (ctx), so there are no mutable globals.
import { useApi } from "./composables/useApi.js";
import { useBadges } from "./composables/useBadges.js";
import { useConfirm } from "./composables/useConfirm.js";
import { useProviders } from "./composables/useProviders.js";
import { useSession } from "./composables/useSession.js";
import { useTheme } from "./composables/useTheme.js";
import { useToast } from "./composables/useToast.js";
import { App } from "./components/App.js";

const { createApp } = window.Vue;
const initial = JSON.parse(document.getElementById("state").textContent);
const api = useApi(!!initial.auth_required);
const ctx = {
  initial, api, shortcuts: {}, openHelp: () => {},
  session: useSession(api, initial.email), toast: useToast(), theme: useTheme(), confirm: useConfirm(),
  badges: useBadges(api), providers: useProviders(api)
};
createApp(App, { ctx }).mount("#app");
