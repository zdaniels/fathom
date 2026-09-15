// Fathom mobile web UI — thread-aware.
//
// Architecture
// ------------
// State lives in three buckets:
//   - localStorage: device token, last-opened thread id (so refresh returns
//     you to where you were)
//   - DOM:          the rendered history of the current thread
//   - in-memory:    `state` object below — token, currentThreadId,
//                   threads list, the live EventSource subscription,
//                   and a Map<messageId, DOMElement> for dedup
//
// Wire flow
// ---------
//   load → fetch /api/v1/threads
//        → if empty: create one
//        → else: open the last-opened or most recent
//   open thread → unsubscribe old SSE → fetch tail history → subscribe SSE
//   send → POST /threads/{id}/messages   (server-of-record)
//        → SSE delivers user_message + agent_done events to ALL devices
//          subscribed to this thread (the Mac sees what the phone sent,
//          and vice versa)
//
// Dedup: messages have stable IDs from the server. Render once per ID;
// SSE re-delivery is a no-op.

(function () {
  const TOKEN_KEY = "fantazm_device_token";
  const DEVICE_KEY = "fantazm_device_meta";
  const LAST_THREAD_KEY = "fantazm_last_thread";

  const $ = (id) => document.getElementById(id);
  const history = $("history");
  const composer = $("composer");
  const input = $("input");
  const sendBtn = $("send-btn");
  const authModal = $("auth-modal");
  const codeInput = $("code-input");
  const deviceNameInput = $("device-name-input");
  const pairBtn = $("pair-btn");
  const tokenInput = $("token-input");
  const tokenBtn = $("token-btn");
  const authErr = $("auth-err");
  const menuBtn = $("menu-btn");
  const drawer = $("drawer");
  const drawerBackdrop = $("drawer-backdrop");
  const drawerClose = $("drawer-close");
  const newSessionBtn = $("new-session-btn");
  const forgetDeviceBtn = $("forget-device-btn");
  const drawerMeta = $("drawer-meta");
  const threadList = $("thread-list");
  const threadTitleEl = $("thread-title");

  const state = {
    token: localStorage.getItem(TOKEN_KEY),
    deviceMeta: JSON.parse(localStorage.getItem(DEVICE_KEY) || "null"),
    currentThreadId: null,
    threads: [],
    sse: null,
    rendered: new Map(), // messageId → DOM element (for dedup)
    pendingUsers: new Map(), // clientMessageId → optimistic DOM element
    threadGeneration: 0,
    inFlight: false,
    // Cross-device thinking indicator: single shared spinner element
    // driven by agent_thinking / agent_done SSE events. Both local
    // send() and remote-originated agent activity reuse it so we
    // don't double-render.
    remoteSpinner: null,
    // Minimal-mode fallback: when the gateway is running with profile=
    // minimal, /api/v1/threads/* returns 503. Boot-time probe of
    // /api/v1/health sets this true so we use the legacy single-shot
    // /api/v1/message path (no threads, no SSE, fresh session per send).
    minimalMode: false,
  };

  // === Auth modal === ====================================================

  function showAuth() {
    authModal.hidden = false;
    setTimeout(() => codeInput.focus(), 80);
  }
  function hideAuth() {
    authModal.hidden = true;
    authErr.hidden = true;
    codeInput.value = "";
    tokenInput.value = "";
    input.focus();
  }
  function setAuthError(msg) {
    authErr.textContent = msg;
    authErr.hidden = false;
  }

  document.querySelectorAll(".modal-tab").forEach((btn) => {
    btn.addEventListener("click", () => {
      const tab = btn.dataset.tab;
      document.querySelectorAll(".modal-tab").forEach((b) => b.classList.toggle("is-active", b === btn));
      document.querySelectorAll(".modal-pane").forEach((p) => {
        p.hidden = p.dataset.pane !== tab;
      });
      const focusable = tab === "code" ? codeInput : tokenInput;
      setTimeout(() => focusable.focus(), 50);
    });
  });

  pairBtn.addEventListener("click", pairDevice);
  codeInput.addEventListener("keydown", (e) => { if (e.key === "Enter") pairDevice(); });
  tokenBtn.addEventListener("click", saveRawToken);
  tokenInput.addEventListener("keydown", (e) => { if (e.key === "Enter") saveRawToken(); });

  async function pairDevice() {
    const code = codeInput.value.trim();
    if (!/^\d{6}$/.test(code)) {
      setAuthError("Code must be 6 digits.");
      return;
    }
    pairBtn.disabled = true;
    try {
      const res = await fetch("api/v1/pair/claim", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          code,
          device_name: deviceNameInput.value.trim() || defaultDeviceName(),
        }),
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        setAuthError(data.error || `Pairing failed (HTTP ${res.status})`);
        return;
      }
      const data = await res.json();
      state.token = data.token;
      state.deviceMeta = { id: data.device_id, name: data.device_name, paired_at: Date.now() };
      localStorage.setItem(TOKEN_KEY, state.token);
      localStorage.setItem(DEVICE_KEY, JSON.stringify(state.deviceMeta));
      hideAuth();
      refreshDrawerMeta();
      await bootThreads();
    } catch (err) {
      setAuthError("Network error: " + err.message);
    } finally {
      pairBtn.disabled = false;
    }
  }

  function saveRawToken() {
    const v = tokenInput.value.trim();
    if (!v) return;
    state.token = v;
    state.deviceMeta = { id: "manual", name: "API token", paired_at: Date.now() };
    localStorage.setItem(TOKEN_KEY, state.token);
    localStorage.setItem(DEVICE_KEY, JSON.stringify(state.deviceMeta));
    hideAuth();
    refreshDrawerMeta();
    bootThreads();
  }

  function defaultDeviceName() {
    const ua = navigator.userAgent;
    if (/iPhone/.test(ua)) return "iPhone";
    if (/iPad/.test(ua)) return "iPad";
    if (/Android/.test(ua)) return "Android";
    if (/Mac/.test(ua)) return "Mac";
    if (/Windows/.test(ua)) return "Windows";
    return "Web";
  }

  // === Drawer === ========================================================

  async function openDrawer() {
    refreshDrawerMeta();
    drawer.hidden = false;
    renderThreadList(); // paint what we have immediately
    // Then refetch in background so threads created on the CLI / Mac
    // menubar / browser since we last opened show up without a hard
    // page reload.
    const res = await apiGet("api/v1/threads");
    if (res && res.threads) {
      state.threads = res.threads;
      renderThreadList();
    }
  }
  function closeDrawer() { drawer.hidden = true; }
  menuBtn.addEventListener("click", openDrawer);
  drawerBackdrop.addEventListener("click", closeDrawer);
  drawerClose.addEventListener("click", closeDrawer);

  // The settings page is reachable from localhost always, and remotely only
  // in team/enterprise mode (the server 404s it over the relay in personal
  // mode). Hide the drawer link by default and reveal it once we know the
  // mode from /health, so personal-mode mobile users never see a dead link
  // but server-deployed enterprise admins do.
  function applySettingsLinkVisibility(mode) {
    const link = $("settings-link");
    if (!link) return;
    const h = location.hostname;
    const local = h === "localhost" || h === "127.0.0.1" || h === "::1" || h === "[::1]";
    link.hidden = !(local || (mode && mode !== "personal"));
  }
  applySettingsLinkVisibility(null); // hidden until /health resolves the mode

  newSessionBtn.addEventListener("click", async () => {
    closeDrawer();
    await createNewThread();
  });

  forgetDeviceBtn.addEventListener("click", () => {
    if (!confirm("Forget this device? You'll need to re-pair.")) return;
    localStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(DEVICE_KEY);
    localStorage.removeItem(LAST_THREAD_KEY);
    state.token = null;
    state.deviceMeta = null;
    closeDrawer();
    teardownSSE();
    showAuth();
  });

  function refreshDrawerMeta() {
    if (!state.deviceMeta) {
      drawerMeta.textContent = "Not paired";
      return;
    }
    drawerMeta.textContent = `${state.deviceMeta.name}\n${location.host}`;
  }

  function renderThreadList() {
    threadList.innerHTML = "";
    if (!state.threads || state.threads.length === 0) {
      const empty = document.createElement("div");
      empty.className = "drawer-empty";
      empty.textContent = "No conversations yet.";
      threadList.appendChild(empty);
      return;
    }
    for (const t of state.threads) {
      const row = document.createElement("div");
      row.className = "thread-row";
      if (t.id === state.currentThreadId) row.classList.add("is-active");

      const main = document.createElement("button");
      main.className = "thread-row-main";
      const title = document.createElement("div");
      title.className = "thread-row-title";
      title.textContent = t.title || "(untitled)";
      const meta = document.createElement("div");
      meta.className = "thread-row-meta";
      meta.textContent = relativeTime(t.updated_at);
      main.appendChild(title);
      main.appendChild(meta);
      main.addEventListener("click", async () => {
        closeDrawer();
        await switchThread(t.id);
      });

      // Trash button — confirm-on-tap to avoid mis-fires on a phone.
      // Soft-delete only; surfaces an undo toast for 6s.
      const trash = document.createElement("button");
      trash.className = "thread-row-trash";
      trash.setAttribute("aria-label", "Delete thread");
      trash.innerHTML = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3,6 5,6 21,6"/><path d="M19 6L18 20a2 2 0 01-2 2H8a2 2 0 01-2-2L5 6"/><path d="M10 11v6M14 11v6"/></svg>`;
      let confirming = false;
      trash.addEventListener("click", async (e) => {
        e.stopPropagation();
        if (!confirming) {
          // First tap arms; second confirms. Reset after 3s.
          confirming = true;
          trash.classList.add("is-confirming");
          setTimeout(() => {
            confirming = false;
            trash.classList.remove("is-confirming");
          }, 3000);
          return;
        }
        await deleteThread(t);
      });

      row.appendChild(main);
      row.appendChild(trash);
      threadList.appendChild(row);
    }
  }

  // deleteThread soft-deletes a thread via the API, removes it from
  // the local state, switches off it if it was current, and surfaces
  // an undo toast for 6 seconds. The toast's Undo button calls
  // /restore which reverses the soft-delete.
  async function deleteThread(thread) {
    try {
      const res = await fetch(`api/v1/threads/${thread.id}`, {
        method: "DELETE",
        headers: { Authorization: "Bearer " + state.token },
      });
      if (res.status === 401) { handleAuthFailure(); return; }
      if (!res.ok) {
        const t = await res.text().catch(() => "");
        appendError(`Delete failed: HTTP ${res.status} — ${t.slice(0, 200)}`);
        return;
      }
    } catch (err) {
      appendError("Delete failed: " + err.message);
      return;
    }
    // Optimistically update local state.
    state.threads = state.threads.filter((t) => t.id !== thread.id);
    renderThreadList();
    // If we were viewing the deleted thread, snap to the next one.
    if (state.currentThreadId === thread.id) {
      const next = state.threads[0];
      if (next) {
        await switchThread(next.id);
      } else {
        await createNewThread();
      }
    }
    showUndoToast(thread);
  }

  // showUndoToast renders a bottom-anchored toast with an Undo button
  // for 6 seconds. Tapping Undo calls POST /restore and re-inserts the
  // thread at the top of the list. Auto-dismisses after the window.
  function showUndoToast(thread) {
    const existing = document.getElementById("undo-toast");
    if (existing) existing.remove();

    const toast = document.createElement("div");
    toast.id = "undo-toast";
    toast.className = "undo-toast";
    toast.innerHTML = `
      <span class="undo-toast-text">Deleted "${escapeHTML(thread.title || "untitled")}"</span>
      <button class="undo-toast-btn" type="button">Undo</button>
    `;
    document.body.appendChild(toast);

    const btn = toast.querySelector(".undo-toast-btn");
    const dismiss = () => {
      toast.classList.add("is-leaving");
      setTimeout(() => toast.remove(), 220);
    };
    const timer = setTimeout(dismiss, 6000);
    btn.addEventListener("click", async () => {
      clearTimeout(timer);
      try {
        await fetch(`api/v1/threads/${thread.id}/restore`, {
          method: "POST",
          headers: { Authorization: "Bearer " + state.token },
        });
        await refreshThreadsListBackgroundAsync();
        renderThreadList();
      } catch (err) {
        appendError("Restore failed: " + err.message);
      } finally {
        dismiss();
      }
    });
  }

  // refreshThreadsListBackgroundAsync is like refreshThreadsListBackground
  // but await-able so undo can wait for the list before re-rendering.
  async function refreshThreadsListBackgroundAsync() {
    const res = await apiGet("api/v1/threads");
    if (res && res.threads) state.threads = res.threads;
  }

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({
      "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&#39;",
    }[c]));
  }

  // === Threads === =======================================================

  async function bootThreads() {
    if (!state.token) {
      showAuth();
      return;
    }
    // Probe /health for the gateway's profile / feature set. Minimal
    // mode skips threads entirely — the chat UI falls back to single-
    // shot /api/v1/message, no drawer, no SSE.
    try {
      const probe = await fetch("api/v1/health");
      if (probe.ok) {
        const h = await probe.json();
        const threadsOn = h && h.features && h.features.threads === true;
        state.minimalMode = !threadsOn;
        applySettingsLinkVisibility(h && h.mode);
      }
    } catch { /* keep state.minimalMode = false; if /threads also 503, send() falls back */ }
    if (state.minimalMode) {
      setTitle("Chat");
      return; // no thread bootstrap, no SSE — composer routes to /message
    }
    try {
      const res = await apiGet("api/v1/threads");
      if (!res) return;
      state.threads = res.threads || [];
      let target =
        localStorage.getItem(LAST_THREAD_KEY) ||
        (state.threads[0] && state.threads[0].id);
      // Validate the LAST_THREAD_KEY is still in the list (deleted on
      // another device, etc.).
      if (target && !state.threads.find((t) => t.id === target)) {
        target = state.threads[0] && state.threads[0].id;
      }
      if (!target) {
        // No threads — create one to land in.
        const created = await apiPost("api/v1/threads", {});
        if (!created) return;
        state.threads.unshift(created);
        target = created.id;
      }
      await switchThread(target);
    } catch (err) {
      appendError("Could not load threads: " + err.message);
    }
  }

  async function createNewThread() {
    const t = await apiPost("api/v1/threads", {});
    if (!t) return;
    // Refresh list + switch.
    state.threads.unshift(t);
    await switchThread(t.id);
  }

  async function switchThread(threadId) {
    if (state.currentThreadId === threadId && state.sse) return;
    teardownSSE();
    const generation = ++state.threadGeneration;
    clearThinking();
    state.currentThreadId = threadId;
    state.rendered.clear();
    state.pendingUsers.clear();
    history.innerHTML = "";
    localStorage.setItem(LAST_THREAD_KEY, threadId);

    // Fetch tail history.
    const res = await apiGet(`api/v1/threads/${threadId}`);
    if (!res || generation !== state.threadGeneration) return;
    setTitle(res.thread && res.thread.title);
    for (const m of res.messages || []) renderMessage(m);

    // Refresh thread list metadata (newer updated_at may have come in).
    refreshThreadsListBackground();

    // Subscribe to live updates.
    subscribeSSE(threadId);
  }

  function refreshThreadsListBackground() {
    apiGet("api/v1/threads").then((res) => {
      if (res && res.threads) state.threads = res.threads;
    }).catch(() => {});
  }

  function setTitle(t) {
    if (!threadTitleEl) return;
    threadTitleEl.textContent = t || "New chat";
  }

  // === SSE === ===========================================================

  function subscribeSSE(threadId) {
    // EventSource doesn't support custom headers, so we can't use it for
    // bearer-token endpoints. Use fetch + ReadableStream instead — same
    // SSE wire format, manual parse.
    const ctrl = new AbortController();
    state.sse = { abort: () => ctrl.abort() };

    fetch(`api/v1/threads/${threadId}/stream`, {
      method: "GET",
      headers: { Authorization: "Bearer " + state.token, Accept: "text/event-stream" },
      signal: ctrl.signal,
    }).then(async (res) => {
      if (res.status === 401) { handleAuthFailure(); return; }
      if (!res.ok) {
        appendError(`Stream error: HTTP ${res.status}`);
        return;
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let leftover = "";
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        const chunk = leftover + decoder.decode(value, { stream: true });
        const events = chunk.split("\n\n");
        leftover = events.pop() || "";
        for (const evt of events) {
          const parsed = parseSSE(evt);
          if (!parsed) continue;
          if (!ctrl.signal.aborted && state.currentThreadId === threadId) handleStreamEvent(parsed);
        }
      }
    }).catch((err) => {
      if (err.name === "AbortError") return;
      // Auto-reconnect after a short delay if the user is still here.
      console.warn("SSE dropped:", err.message);
      setTimeout(() => {
        if (!ctrl.signal.aborted && state.currentThreadId === threadId) subscribeSSE(threadId);
      }, 2000);
    });
  }

  function teardownSSE() {
    if (state.sse) {
      state.sse.abort();
      state.sse = null;
    }
  }

  function handleStreamEvent(evt) {
    switch (evt.event) {
      case "ready":
        break;
      case "user_message":
        if (evt.data && evt.data.message) renderMessage(evt.data.message);
        break;
      case "agent_thinking":
        // Cross-device thinking indicator: another device (or this
        // one) just sent a message and the agent is working on it.
        // Show the spinner if we don't already have one in flight.
        ensureThinking();
        break;
      case "agent_done":
        clearThinking();
        if (evt.data && evt.data.message) renderMessage(evt.data.message);
        break;
      case "agent_error":
        clearThinking();
        appendError(evt.data && evt.data.error || "Agent error");
        break;
      default:
        // ignore unknown event types
    }
  }

  function parseSSE(block) {
    let event = "message";
    let data = null;
    for (const line of block.split("\n")) {
      if (line.startsWith("event:")) event = line.slice(6).trim();
      else if (line.startsWith("data:")) {
        const raw = line.slice(5).trim();
        try { data = JSON.parse(raw); } catch { data = { text: raw }; }
      }
    }
    return data != null || event !== "message" ? { event, data } : null;
  }

  // === Rendering === =====================================================

  function scrollToBottom() {
    history.scrollTop = history.scrollHeight;
  }

  // renderMessage is the dedup gate. Skips if we've already drawn this
  // message ID — that way SSE re-deliveries of a message we POSTed are
  // silently ignored.
  function renderMessage(m) {
    if (!m || !m.id || (m.thread_id && m.thread_id !== state.currentThreadId)) return;
    if (state.rendered.has(m.id)) return;
    const clientId = m.metadata && m.metadata.clientMessageId;
    const pending = m.role === "user" && state.pendingUsers.get(clientId);
    if (pending) {
      state.pendingUsers.delete(clientId);
      state.rendered.delete(clientId);
      state.rendered.set(m.id, pending);
      return;
    }
    const turn = document.createElement("div");
    if (m.role === "user") {
      turn.className = "turn-user";
      const b = document.createElement("div");
      b.className = "bubble";
      b.textContent = m.content;
      turn.appendChild(b);
    } else {
      turn.className = "turn-agent";
      const c = document.createElement("div");
      c.className = "content";
      renderMarkdown(c, m.content);
      turn.appendChild(c);
    }
    history.appendChild(turn);
    state.rendered.set(m.id, turn);
    scrollToBottom();
    return turn;
  }

  // Words the spinner cycles through. "thinking…" is the workhorse;
  // "fathoming…" is the wink at the brand — shows roughly 1 in 4
  // ticks so it's a treat, not a gimmick.
  const SPINNER_WORDS = ["thinking…", "thinking…", "fathoming…", "thinking…"];

  function ensureThinking() {
    if (!state.remoteSpinner) state.remoteSpinner = appendSpinner();
    return state.remoteSpinner;
  }

  function clearThinking(expected = state.remoteSpinner) {
    if (expected) expected.remove();
    if (state.remoteSpinner === expected) state.remoteSpinner = null;
  }

  function appendSpinner() {
    const t = document.createElement("div");
    t.className = "turn-agent";
    t.dataset.spinner = "1";
    const s = document.createElement("div");
    s.className = "spinner";
    // Real menubar icon as the spinner glyph — same PNG the macOS
    // app ships, so the mark is pixel-identical across surfaces. CSS
    // tints + pulses it.
    s.innerHTML = `
      <span class="iceberg-glyph" role="img" aria-label="thinking"></span>
      <span class="spinner-label">thinking…</span>`;
    t.appendChild(s);
    history.appendChild(t);
    scrollToBottom();

    // Cycle the label every ~3s while the spinner is visible.
    const label = s.querySelector(".spinner-label");
    let i = 0;
    const interval = setInterval(() => {
      i = (i + 1) % SPINNER_WORDS.length;
      label.textContent = SPINNER_WORDS[i];
    }, 3000);
    // Patch .remove() so callers (existing flow) automatically stop the
    // interval when they remove the spinner element.
    const origRemove = t.remove.bind(t);
    t.remove = () => {
      clearInterval(interval);
      origRemove();
    };
    return t;
  }

  function appendError(text) {
    const e = document.createElement("div");
    e.className = "error-banner";
    e.textContent = text;
    history.appendChild(e);
    scrollToBottom();
  }

  // Markdown — same minimal renderer as before.
  function renderMarkdown(target, text) {
    target.textContent = "";
    const lines = text.split("\n");
    let i = 0;
    while (i < lines.length) {
      const line = lines[i];
      if (line.startsWith("```")) {
        const lang = line.slice(3).trim();
        const buf = [];
        i++;
        while (i < lines.length && !lines[i].startsWith("```")) {
          buf.push(lines[i]);
          i++;
        }
        const pre = document.createElement("pre");
        const code = document.createElement("code");
        if (lang) code.className = "lang-" + lang;
        code.textContent = buf.join("\n");
        pre.appendChild(code);
        target.appendChild(pre);
        i++;
        continue;
      }
      const para = document.createElement("div");
      renderInlineInto(para, line);
      if (i < lines.length - 1) para.appendChild(document.createElement("br"));
      target.appendChild(para);
      i++;
    }
  }

  function renderInlineInto(target, text) {
    const re = /(\*\*[^*]+\*\*|\*[^*]+\*|`[^`]+`)/g;
    let last = 0;
    let m;
    while ((m = re.exec(text)) !== null) {
      if (m.index > last) target.appendChild(document.createTextNode(text.slice(last, m.index)));
      const tok = m[0];
      if (tok.startsWith("**")) {
        const s = document.createElement("strong");
        s.textContent = tok.slice(2, -2);
        target.appendChild(s);
      } else if (tok.startsWith("`")) {
        const c = document.createElement("code");
        c.textContent = tok.slice(1, -1);
        target.appendChild(c);
      } else {
        const e = document.createElement("em");
        e.textContent = tok.slice(1, -1);
        target.appendChild(e);
      }
      last = m.index + tok.length;
    }
    if (last < text.length) target.appendChild(document.createTextNode(text.slice(last)));
  }

  // === Sending === =======================================================

  async function send(text) {
    if (state.inFlight) return;

    // Minimal-mode bypass: no threads, just a one-shot POST to the
    // legacy endpoint. Render locally — no SSE delivery to dedup
    // against in this path.
    if (state.minimalMode) {
      state.inFlight = true;
      sendBtn.disabled = true;
      const spinner = appendSpinner();
      try {
        const res = await fetch("api/v1/message", {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: "Bearer " + state.token,
          },
          body: JSON.stringify({ text }),
        });
        spinner.remove();
        if (res.status === 401) { handleAuthFailure(); return; }
        if (!res.ok) {
          const t = await res.text().catch(() => "");
          appendError(`HTTP ${res.status}: ${t.slice(0, 200)}`);
          return;
        }
        const data = await res.json();
        // Synthesize message records to feed renderMessage's dedup map.
        const now = new Date().toISOString();
        const userMsg = { id: "u-" + Date.now(), role: "user", content: text, created_at: now };
        const agentMsg = { id: "a-" + Date.now(), role: "agent", content: data.response || "(empty response)", created_at: now };
        renderMessage(userMsg);
        renderMessage(agentMsg);
      } catch (err) {
        spinner.remove();
        appendError("Network error: " + err.message);
      } finally {
        state.inFlight = false;
        sendBtn.disabled = false;
      }
      return;
    }

    if (!state.currentThreadId) {
      // Race protection — if somehow there's no active thread (e.g.
      // create failed on boot), make one now.
      await createNewThread();
      if (!state.currentThreadId) {
        appendError("No active thread");
        return;
      }
    }
    state.inFlight = true;
    sendBtn.disabled = true;
    // Correlate the optimistic bubble with the persisted message across both
    // response paths, regardless of whether SSE or the POST completes first.
    const localId = "local-" + Date.now() + "-" + Math.random().toString(36).slice(2, 8);
    const now = new Date().toISOString();
    const threadId = state.currentThreadId;
    const generation = state.threadGeneration;
    const optimistic = renderMessage({ id: localId, role: "user", content: text, created_at: now });
    state.pendingUsers.set(localId, optimistic);
    const spinner = ensureThinking();

    try {
      const res = await fetch(`api/v1/threads/${threadId}/messages`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: "Bearer " + state.token,
        },
        body: JSON.stringify({ text, clientMessageId: localId }),
      });
      clearThinking(spinner);
      if (generation !== state.threadGeneration) return;
      if (res.status === 401) { handleAuthFailure(); return; }
      if (!res.ok) {
        const t = await res.text().catch(() => "");
        appendError(`HTTP ${res.status}: ${t.slice(0, 200)}`);
        return;
      }
      const data = await res.json();
      if (generation !== state.threadGeneration) return;
      if (data.user_message) renderMessage(data.user_message);
      if (data.agent_message) renderMessage(data.agent_message);
      // Auto-title may have updated the topbar.
      if (data.agent_message) {
        // Refresh title from server (cheap, one round trip).
        apiGet(`api/v1/threads/${threadId}`).then((meta) => {
          if (generation === state.threadGeneration && meta && meta.thread) setTitle(meta.thread.title);
        }).catch(() => {});
      }
    } catch (err) {
      clearThinking(spinner);
      appendError("Network error: " + err.message);
    } finally {
      state.inFlight = false;
      sendBtn.disabled = false;
    }
  }

  function handleAuthFailure() {
    localStorage.removeItem(TOKEN_KEY);
    state.token = null;
    teardownSSE();
    appendError("Token rejected. Pair again.");
    showAuth();
  }

  // === Composer wiring === ===============================================

  function autosize() {
    input.style.height = "auto";
    input.style.height = Math.min(input.scrollHeight, window.innerHeight * 0.35) + "px";
  }
  input.addEventListener("input", autosize);

  composer.addEventListener("submit", (e) => {
    e.preventDefault();
    const text = input.value.trim();
    if (!text) return;
    input.value = "";
    autosize();
    send(text);
  });

  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && !("ontouchstart" in window)) {
      e.preventDefault();
      composer.dispatchEvent(new Event("submit"));
    }
  });

  // === API helpers === ===================================================

  async function apiGet(url) {
    try {
      const res = await fetch(url, {
        headers: { Authorization: "Bearer " + state.token },
      });
      if (res.status === 401) { handleAuthFailure(); return null; }
      if (!res.ok) {
        const t = await res.text().catch(() => "");
        appendError(`GET ${url}: HTTP ${res.status} — ${t.slice(0, 200)}`);
        return null;
      }
      return await res.json();
    } catch (err) {
      appendError(`GET ${url}: ${err.message}`);
      return null;
    }
  }

  async function apiPost(url, body) {
    try {
      const res = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json", Authorization: "Bearer " + state.token },
        body: JSON.stringify(body || {}),
      });
      if (res.status === 401) { handleAuthFailure(); return null; }
      if (!res.ok) {
        const t = await res.text().catch(() => "");
        appendError(`POST ${url}: HTTP ${res.status} — ${t.slice(0, 200)}`);
        return null;
      }
      return await res.json();
    } catch (err) {
      appendError(`POST ${url}: ${err.message}`);
      return null;
    }
  }

  // === Service worker === ================================================

  if ("serviceWorker" in navigator) {
    window.addEventListener("load", () => {
      navigator.serviceWorker.register("sw.js").catch((err) => {
        console.warn("SW registration failed:", err);
      });
    });
  }

  // === Helpers === =======================================================

  function relativeTime(ts) {
    if (!ts) return "";
    const t = new Date(ts).getTime();
    if (isNaN(t)) return "";
    const d = (Date.now() - t) / 1000;
    if (d < 60) return "just now";
    if (d < 3600) return `${Math.floor(d / 60)}m ago`;
    if (d < 86400) return `${Math.floor(d / 3600)}h ago`;
    return `${Math.floor(d / 86400)}d ago`;
  }

  // === Boot === ==========================================================

  // ?pair_code=… in URL: prefill the auth modal even if a token already
  // exists, so a fresh pairing replaces a stale one.
  const urlParams = new URLSearchParams(location.search);
  const incomingCode = urlParams.get("pair_code");
  if (incomingCode) {
    history.replaceState && history.replaceState({}, "", location.pathname + location.hash);
  }

  if (!state.token || incomingCode) {
    if (incomingCode) {
      setTimeout(() => { codeInput.value = incomingCode.replace(/\D/g, "").slice(0, 6); }, 100);
    }
    showAuth();
  } else {
    bootThreads();
  }
  refreshDrawerMeta();

  if (!("ontouchstart" in window)) input.focus();
})();
