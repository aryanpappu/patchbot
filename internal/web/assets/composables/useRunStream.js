// Live run log over fetch-based SSE. Frames are parsed across chunk boundaries,
// sequence IDs are deduplicated, only fully consumed frames advance the cursor,
// and reconnects send Last-Event-ID with a 1-10 s backoff. Keeps the latest
// 1,000 events.
const { reactive } = window.Vue;

export function useRunStream(api, runId, onEvent, onDrained) {
  const state = reactive({ events: [], status: "Connecting", lastID: 0 });
  let controller = null, disposed = false;

  function consume(frame) {
    let id = "", type = "message";
    const data = [];
    for (const line of frame.split("\n")) {
      const colon = line.indexOf(":");
      const key = colon < 0 ? line : line.slice(0, colon);
      const value = colon < 0 ? "" : line.slice(colon + 1).replace(/^ /, "");
      if (key === "id") id = value;
      if (key === "event") type = value;
      if (key === "data") data.push(value);
    }
    if (type === "end") {
      try {
        const end = JSON.parse(data.join("\n"));
        return Number.isSafeInteger(end.last_seq) && end.last_seq === state.lastID;
      } catch { return false; }
    }
    if (!data.length || !/^\d+$/.test(id) || !Number.isSafeInteger(Number(id)) || Number(id) <= state.lastID) return false;
    const payload = data.join("\n");
    let output = payload;
    try { output = JSON.stringify(JSON.parse(payload), null, 2); } catch { /* Malformed data is shown as text. */ }
    state.events.push({ id, type, output });
    if (state.events.length > 1000) state.events.shift();
    state.lastID = Number(id);
    onEvent();
    return false;
  }

  async function follow(signal) {
    let delay = 1000;
    while (!signal.aborted && !api.state.locked && !disposed) {
      try {
        state.status = state.lastID ? "Reconnecting" : "Connecting";
        const extra = { Accept: "text/event-stream" };
        if (state.lastID) extra["Last-Event-ID"] = String(state.lastID);
        const response = await fetch(`/api/runs/${runId}/events`, { credentials: "same-origin", headers: api.headers(extra), signal });
        if (response.status === 401) { if (api.state.authRequired) api.forget(); throw new Error("Sign in to resume the log."); }
        if (!response.ok || !response.body) throw new Error(`Live log unavailable (${response.status}).`);
        state.status = "Live";
        const reader = response.body.getReader(), decoder = new TextDecoder();
        let pending = "", frame = [], complete = false;
        try {
          while (!signal.aborted && !complete) {
            const part = await reader.read();
            if (part.done) break;
            pending += decoder.decode(part.value, { stream: true });
            let newline;
            while ((newline = pending.indexOf("\n")) >= 0) {
              const line = pending.slice(0, newline).replace(/\r$/, "");
              pending = pending.slice(newline + 1);
              if (line === "") {
                const before = state.lastID;
                complete = consume(frame.join("\n")); frame = [];
                if (state.lastID > before) delay = 1000;
                if (complete) break;
              } else frame.push(line);
            }
          }
        } finally { await reader.cancel(); reader.releaseLock(); }
        await onDrained();
        // Only an explicit end frame proves terminal replay was drained.
        if (complete) { state.status = "Complete"; return; }
        state.status = "Reconnecting";
      } catch (error) {
        if (signal.aborted || disposed) return;
        state.status = error.message + " Reconnecting…";
      }
      if (api.state.locked) return;
      await new Promise(resolve => setTimeout(resolve, delay));
      delay = Math.min(delay * 2, 10000);
    }
  }

  function start() {
    stop();
    state.lastID = 0; state.events = [];
    controller = new AbortController();
    void follow(controller.signal);
  }
  function stop() { if (controller) controller.abort(); controller = null; }
  function dispose() { disposed = true; stop(); }
  return { state, start, stop, dispose };
}
