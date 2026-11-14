// Connect GitHub page script. Plain DOM only; every dynamic value is set with
// textContent or as a form field value, never as HTML.
(() => {
  const storageKey = "patchbot.ui-token";
  const headers = (extra) => {
    let token = "";
    try { token = sessionStorage.getItem(storageKey) || ""; } catch { /* no storage: unauthenticated mode only */ }
    return token ? { ...extra, Authorization: "Bearer " + token } : extra;
  };
  const say = (id, message) => {
    const node = document.getElementById(id);
    if (node) { node.textContent = message; }
  };
  const failure = (body, status) => {
    if (status === 401) { return "Sign in first: open the Runs page and enter the UI token, then come back."; }
    return (body && body.error && body.error.message) || "Request failed.";
  };

  const connect = document.getElementById("connect");
  if (connect) {
    connect.addEventListener("click", async () => {
      connect.disabled = true;
      say("setup-status", "Preparing the GitHub request…");
      try {
        const response = await fetch("/api/github/setup/start", { method: "POST", headers: headers({}) });
        const body = await response.json().catch(() => null);
        if (!response.ok || !body || !body.action || !body.manifest) {
          say("setup-status", failure(body, response.status));
          connect.disabled = false;
          return;
        }
        const form = document.createElement("form");
        form.method = "post";
        form.action = body.action;
        const field = document.createElement("input");
        field.type = "hidden";
        field.name = "manifest";
        field.value = body.manifest;
        form.append(field);
        document.body.append(form);
        form.submit();
      } catch {
        say("setup-status", "Could not reach patchbot.");
        connect.disabled = false;
      }
    });
  }

  const recheck = document.getElementById("recheck");
  if (recheck) {
    recheck.addEventListener("click", async () => {
      recheck.disabled = true;
      say("recheck-status", "Checking…");
      try {
        const response = await fetch("/api/github/status", { headers: headers({}) });
        const body = await response.json().catch(() => null);
        if (!response.ok || !body) {
          say("recheck-status", failure(body, response.status));
        } else if (!body.connected) {
          say("recheck-status", "Not connected yet.");
        } else if (body.installations_error) {
          say("recheck-status", "Connected as " + body.slug + ". " + body.installations_error);
        } else if (!body.installations || body.installations.length === 0) {
          say("recheck-status", "Connected as " + body.slug + ", but not installed on any account yet. Use the install link above.");
        } else {
          const where = body.installations.map((i) => i.account + " (" + i.repository_selection + " repositories)").join(", ");
          say("recheck-status", "Connected as " + body.slug + ". Installed on: " + where +
            (body.restart_required ? ". Restart patchbot to start accepting runs." : "."));
        }
      } catch {
        say("recheck-status", "Could not reach patchbot.");
      }
      recheck.disabled = false;
    });
  }
})();
