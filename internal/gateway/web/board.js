(() => {
  "use strict";
  const $ = (id) => document.getElementById(id),
    state = { workspace: "", models: [], data: null, task: null, busy: false };
  const node = (tag, text, cls) => {
    const e = document.createElement(tag);
    e.textContent = text;
    if (cls) e.className = cls;
    return e;
  };

  function markdown(target, text) {
    const inline = (el, value) => {
      const re = /(\*\*[^*]+\*\*|`[^`]+`)/g;
      let cursor = 0;
      for (const match of value.matchAll(re)) {
        el.append(document.createTextNode(value.slice(cursor, match.index)));
        el.append(
          node(
            match[0].startsWith("**") ? "strong" : "code",
            match[0].startsWith("**")
              ? match[0].slice(2, -2)
              : match[0].slice(1, -1),
          ),
        );
        cursor = match.index + match[0].length;
      }
      el.append(document.createTextNode(value.slice(cursor)));
    };
    target.replaceChildren();
    const lines = String(text).split("\n");
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i].trim();
      if (line.startsWith("```")) {
        const code = [];
        while (++i < lines.length && !lines[i].trim().startsWith("```"))
          code.push(lines[i]);
        const pre = node("pre", "");
        pre.append(node("code", code.join("\n")));
        target.append(pre);
        continue;
      }
      if (
        line.startsWith("|") &&
        /^\|?[\s:|-]+\|$/.test((lines[i + 1] || "").trim())
      ) {
        const table = node("table", "");
        const row = (value, header) => {
          const tr = node("tr", "");
          for (const cell of value
            .trim()
            .replace(/^\||\|$/g, "")
            .split("|")) {
            const td = node(header ? "th" : "td", "");
            inline(td, cell.trim());
            tr.append(td);
          }
          table.append(tr);
        };
        row(line, true);
        i++;
        while (i + 1 < lines.length && lines[i + 1].trim().startsWith("|"))
          row(lines[++i], false);
        target.append(table);
        continue;
      }
      const p = node(/^#{1,3} /.test(line) ? "h3" : "p", "");
      inline(p, line.replace(/^#{1,3} /, ""));
      target.append(p);
    }
  }
  function headers() {
    return {
      Authorization:
        "Bearer " + (localStorage.getItem("fantazm_device_token") || ""),
      "Content-Type": "application/json",
    };
  }
  async function api(path = "", body) {
    const r = await fetch("/api/v1/board" + path, {
      method: body === undefined ? "GET" : "POST",
      headers: (() => {
        const h = headers();
        if (body !== undefined && /\/connections\//.test(path)) {
          try {
            const grant = JSON.parse(
              sessionStorage.getItem("fathom_step_up") || "null",
            );
            if (grant && grant.expires > Date.now()) {
              h["X-Step-Up-Token"] = grant.token;
              sessionStorage.removeItem("fathom_step_up");
            }
          } catch {}
        }
        return h;
      })(),
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const data = await r.json();
    if (!r.ok) throw Error(data.error || `Request failed (${r.status})`);
    return data;
  }
  const report = (e) => {
    $("status").textContent = e.message;
    $("task-status").textContent = e.message;
  };
  function options(select, values, empty) {
    select.replaceChildren();
    if (empty !== undefined) {
      const o = node("option", empty);
      o.value = "";
      select.append(o);
    }
    for (const value of values) {
      const o = node("option", value);
      o.value = value;
      select.append(o);
    }
  }
  async function init() {
    const data = await api();
    state.models = data.models;
    $("run-setup").textContent =
      data.runSetup ||
      "Runs use offline containers. You start each builder and reviewer; completion requires human approval.";
    const current = state.workspace;
    $("workspace").replaceChildren();
    for (const w of data.workspaces) {
      const o = node("option", w.name);
      o.value = w.id;
      $("workspace").append(o);
    }
    state.workspace = data.workspaces.some((w) => w.id === current)
      ? current
      : data.workspaces[0]?.id || "";
    $("workspace").value = state.workspace;
    state.data = null;
    startLive();
    await refresh();
  }
  let refreshing = null,
    refreshAgain = false;
  function refresh() {
    refreshAgain = true;
    if (refreshing) return refreshing;
    refreshing = (async () => {
      while (refreshAgain) {
        refreshAgain = false;
        if (!state.workspace) {
          $("workspace-content").hidden = true;
          continue;
        }
        const id = state.workspace;
        const data = await api("/" + id);
        if (id !== state.workspace) continue;
        if (state.data && JSON.stringify(state.data) === JSON.stringify(data))
          continue;
        state.data = data;
        $("workspace-content").hidden = false;
        render();
      }
    })().finally(() => {
      refreshing = null;
    });
    return refreshing;
  }
  let liveController = null,
    liveTimer = null,
    liveConnected = false;
  function stopLive() {
    clearTimeout(liveTimer);
    liveTimer = null;
    liveController?.abort();
    liveController = null;
    liveConnected = false;
  }
  function startLive() {
    stopLive();
    if (!state.workspace) {
      $("live-status").textContent = "";
      return;
    }
    const id = state.workspace,
      controller = new AbortController();
    liveController = controller;
    const current = () =>
      liveController === controller && state.workspace === id;
    $("live-status").textContent = "Connecting live updates…";
    (async () => {
      let retry = true;
      try {
        const response = await fetch(`/api/v1/board/${id}/events`, {
          headers: headers(),
          signal: controller.signal,
        });
        if (!current()) return;
        if ([401, 403, 404].includes(response.status)) {
          retry = false;
          state.task = null;
          state.data = null;
          state.workspace = "";
          $("task-dialog").close();
          closeConnections();
          $("workspace-content").hidden = true;
          $("live-status").textContent =
            "Access ended. Sign in again or choose another workspace.";
          return;
        }
        if (!response.ok || !response.body)
          throw Error("Live connection unavailable");
        liveConnected = true;
        $("live-status").textContent = "Live updates connected";
        const reader = response.body.getReader(),
          decoder = new TextDecoder();
        let buffer = "";
        try {
          while (current()) {
            const { value, done } = await reader.read();
            if (done) break;
            buffer += decoder.decode(value, { stream: true });
            let boundary;
            while ((boundary = buffer.indexOf("\n\n")) >= 0) {
              const event = buffer.slice(0, boundary);
              buffer = buffer.slice(boundary + 2);
              if (event.split("\n").includes("event: changed"))
                refresh().catch(report);
            }
            if (buffer.length > 4096) throw Error("Invalid live update");
          }
        } finally {
          await reader.cancel().catch(() => {});
        }
      } catch (error) {
        if (controller.signal.aborted) return;
      } finally {
        if (current()) {
          liveConnected = false;
          if (retry && !controller.signal.aborted) {
            $("live-status").textContent = "Reconnecting live updates…";
            liveTimer = setTimeout(startLive, 2000);
          }
        }
      }
    })();
  }
  function render() {
    const data = state.data,
      write = data.role !== "viewer";
    $("workspace-id").textContent = "Workspace ID: " + state.workspace;
    $("member-form").hidden = data.role !== "admin";
    $("task-form").querySelector("button").disabled = !write;
    $("members").replaceChildren(
      ...data.members.map((m) =>
        node("div", m.userId + " · " + m.role, "person"),
      ),
    );
    $("board").replaceChildren();
    for (const key of [
      "backlog",
      "ready",
      "running",
      "review",
      "blocked",
      "done",
    ]) {
      const lane = node("div", "", "lane");
      lane.append(node("h2", key[0].toUpperCase() + key.slice(1)));
      for (const t of data.tasks.filter((t) => t.state === key)) {
        const card = node("button", "", "task");
        card.style.width = "100%";
        card.style.textAlign = "left";
        card.append(
          node("strong", t.title),
          node("small", t.assignee || "Unassigned"),
        );
        if (t.source) card.append(node("small", " · " + t.source));
        card.onclick = () => openTask(t);
        lane.append(card);
      }
      $("board").append(lane);
    }
    options(
      $("connection"),
      data.connections.map((c) => c.provider),
    );
    $("manage-connections").hidden = !data.canManageConnections;
    if (!data.canManageConnections && $("connections-dialog").open)
      closeConnections();
    $("import-form").hidden = !data.connections.length || !write;
    $("connections-note").textContent = data.connections.length
      ? data.connections.map((c) => c.provider + " · " + c.scope).join(", ")
      : "No connection configured. The built-in board is ready to use. Your instance administrator can connect this workspace to a Linear team or Jira project.";
    renderActivity($("activity"), data.activity);
    if (state.task) {
      renderActivity(
        $("task-activity"),
        data.activity.filter((e) => e.taskId === state.task.id),
      );
      updateTaskControls();
      const latest = data.tasks.find((t) => t.id === state.task.id);
      if (latest && latest.revision !== state.task.revision && !state.busy) {
        if (state.task.state === "running") {
          openTask(latest, true);
          return;
        }
        $("task-status").textContent =
          "This task changed. Close and reopen it before editing.";
      }
    }
  }
  function renderActivity(target, events) {
    target.replaceChildren(
      ...events.map((e) => {
        const row = node("div", "", "activity");
        row.append(
          node(
            "small",
            new Date(e.at).toLocaleString() + " · " + e.actor + " · " + e.kind,
          ),
          node("pre", e.text),
        );
        return row;
      }),
    );
  }
  // A compact replacement hunk: unchanged prefix/suffix are collapsed. File
  // content always goes through textContent, including markup in source files.
  function fileDiff(target, file) {
    if (!file.preview) {
      target.append(
        node("p", "Text preview unavailable (binary, link, or preview limit)."),
      );
      return;
    }
    const before = file.before ? file.before.split("\n") : [];
    const after = file.after ? file.after.split("\n") : [];
    let start = 0,
      end = 0;
    while (
      start < before.length &&
      start < after.length &&
      before[start] === after[start]
    )
      start++;
    while (
      end < before.length - start &&
      end < after.length - start &&
      before[before.length - 1 - end] === after[after.length - 1 - end]
    )
      end++;
    const pre = node("pre", "", "file-diff");
    const line = (prefix, text, cls) =>
      pre.append(node("span", prefix + text + "\n", cls));
    if (start > 3) line(" ", `… ${start - 3} unchanged lines …`, "muted");
    for (let i = Math.max(0, start - 3); i < start; i++) line(" ", before[i]);
    for (let i = start; i < before.length - end; i++)
      line("-", before[i], "diff-removed");
    for (let i = start; i < after.length - end; i++)
      line("+", after[i], "diff-added");
    for (
      let i = after.length - end;
      i < Math.min(after.length, after.length - end + 3);
      i++
    )
      line(" ", after[i]);
    if (end > 3) line(" ", `… ${end - 3} unchanged lines …`, "muted");
    if (file.before === file.after)
      target.append(
        node(
          "p",
          file.kind === "modified"
            ? "No text changes (file metadata changed)."
            : "Empty file.",
        ),
      );
    else target.append(pre);
  }
  async function loadEvidence(t) {
    const workspace = state.workspace;
    const target = $("run-evidence");
    target.replaceChildren(node("p", "Loading recorded evidence…"));
    try {
      const records = await api(`/${workspace}/tasks/${t.id}/evidence`);
      if (state.workspace !== workspace || state.task !== t) return;
      target.replaceChildren();
      if (!records.length)
        target.append(
          node(
            "p",
            t.state === "running"
              ? "Evidence will appear when this run finishes."
              : "No recorded evidence yet. Start a new builder run to capture file changes and test results.",
          ),
        );
      for (const run of records) {
        target.append(
          node(
            "h3",
            `${run.role === "builder" ? "Builder" : "Reviewer"} · ${new Date(run.finishedAt).toLocaleString()}`,
          ),
        );
        if (run.error)
          target.append(
            node("p", "Run failed: " + run.error, "evidence-warning"),
          );
        if (run.warning)
          target.append(node("p", run.warning, "evidence-warning"));
        if (run.role === "builder") {
          target.append(
            node(
              "p",
              run.warning
                ? "File comparison is incomplete."
                : `${run.changes.length} changed files`,
            ),
          );
          for (const file of run.changes) {
            const details = node("details", "", "evidence-file");
            details.append(node("summary", `${file.kind} · ${file.path}`));
            let rendered = false;
            details.ontoggle = () => {
              if (!details.open || rendered) return;
              rendered = true;
              details.append(
                node(
                  "small",
                  `Permissions: ${file.beforeMode.toString(8)} → ${file.afterMode.toString(8)}`,
                ),
              );
              fileDiff(details, file);
            };
            target.append(details);
          }
        }
        const tests = run.commands.filter((c) => c.kind === "test");
        target.append(
          node(
            "p",
            tests.length
              ? `${tests.length} recorded test commands · ${tests.filter((c) => c.exitCode !== 0).length} with nonzero exit status`
              : "No test commands recorded. Shell commands below may contain additional checks.",
          ),
        );
        target.append(
          node(
            "small",
            "Exit 0 means the command succeeded; it does not guarantee correctness.",
          ),
        );
        for (const command of run.commands) {
          const details = node("details", "", "evidence-command");
          details.append(
            node(
              "summary",
              `${command.kind === "test" ? "Test" : "Shell"} · exit ${command.exitCode} · ${(command.durationMs / 1000).toFixed(1)}s · ${command.command.split("\n")[0].slice(0, 100)}`,
            ),
          );
          details.append(node("pre", "$ " + command.command));
          details.append(node("pre", command.output || "(no output)"));
          if (command.truncated)
            details.append(
              node("p", "Command or output shortened for display.", "muted"),
            );
          target.append(details);
        }
      }
    } catch (error) {
      if (state.workspace === workspace && state.task === t)
        target.replaceChildren(
          node("p", "Could not load evidence: " + error.message),
        );
    }
  }
  function openTask(t, preserveComment = false) {
    const draft = preserveComment ? $("task-comment").value : "";
    state.task = t;
    loadEvidence(t);
    $("task-status").textContent = "";
    $("edit-title").value = t.title;
    $("edit-description").value = t.description;
    options(
      $("edit-assignee"),
      state.data.members.map((m) => m.userId),
      "Unassigned",
    );
    $("edit-assignee").value = t.assignee;
    for (const field of ["builder", "reviewer"]) {
      options($("edit-" + field), state.models, "Default model");
      $("edit-" + field).value = t[field];
    }
    $("task-state").textContent =
      "State: " + t.state + " · revision " + t.revision;
    markdown($("handoff"), t.handoff || "No builder handoff yet.");
    markdown($("review"), t.review || "No review yet.");
    $("publish-preview").hidden = true;
    if ($("task-comment").value !== draft) $("task-comment").value = draft;
    const link = $("external-link");
    link.hidden = true;
    try {
      const url = new URL(t.externalUrl);
      if (url.protocol === "https:") {
        link.href = url.href;
        link.hidden = false;
      }
    } catch {}
    $("publish").hidden = !t.source || !t.handoff;
    renderActivity(
      $("task-activity"),
      state.data.activity.filter((e) => e.taskId === t.id),
    );
    updateTaskControls();
    if (!$("task-dialog").open) $("task-dialog").showModal();
  }
  function updateTaskControls() {
    const t = state.task;
    if (!t || !state.data) return;
    const readOnly = state.data.role === "viewer";
    for (const el of $("task-dialog").querySelectorAll(
      "input,textarea,select,button[data-action]",
    ))
      el.disabled = readOnly || t.state === "running" || state.busy;
    // Discussion stays editable while an agent is running or a comment is
    // sending. Only clear the text that the successful request actually sent.
    $("task-comment").disabled = readOnly;
    $("task-dialog").querySelector('[data-action="comment"]').disabled =
      readOnly || state.busy;
    $("edit-form").querySelector("button").disabled =
      readOnly || t.state === "running" || state.busy;
    $("task-dialog").querySelector('[data-action="cancel"]').disabled =
      readOnly || t.state !== "running" || state.busy;
    $("task-dialog").querySelector('[data-action="approve"]').disabled =
      readOnly || t.state !== "review" || !t.review || state.busy;
  }
  async function action(name, extra = {}) {
    if (state.busy || !state.task) return;
    state.busy = true;
    updateTaskControls();
    const workspace = state.workspace,
      task = state.task.id,
      text = $("task-comment").value;
    const current = () =>
      state.workspace === workspace && state.task?.id === task;
    try {
      const t = await api(`/${workspace}/tasks/${task}/${name}`, {
        revision: state.task.revision,
        text,
        ...extra,
      });
      if (!current()) return;
      if (name === "comment" && $("task-comment").value === text)
        $("task-comment").value = "";
      await refresh();
      if (!current()) return;
      if (t.id) openTask(t, true);
      else $("task-status").textContent = t.status || "Saved.";
    } catch (e) {
      if (current()) report(e);
    } finally {
      state.busy = false;
      // A live completion may have arrived while this POST was in flight.
      if (state.data) render();
      updateTaskControls();
    }
  }
  let connectionWorkspace = "",
    connectionRecords = [],
    connectionBusy = false;
  function closeConnections() {
    $("connections-dialog").close();
    $("connection-token").value = "";
    connectionWorkspace = "";
  }
  function fillConnection() {
    const provider = $("connection-provider").value;
    const saved = connectionRecords.find((c) => c.provider === provider);
    $("connection-scope").value = saved?.scope || "";
    $("connection-site").value = saved?.site || "";
    $("connection-email").value = saved?.email || "";
    $("connection-token").value = "";
    $("connection-token").required = !saved?.credentialSaved;
    const jira = provider === "jira";
    $("connection-scope-label").textContent = jira
      ? "Jira project key"
      : "Linear team UUID";
    $("connection-scope").placeholder = jira
      ? "TEAM"
      : "Team UUID from Linear settings";
    for (const field of ["site", "email"]) {
      $("connection-" + field + "-field").hidden = !jira;
      $("connection-" + field).required = jira;
    }
    $("connection-disable").disabled = !saved?.enabled;
    $("connection-saved").textContent = saved
      ? `${saved.enabled ? "Enabled" : "Disabled"} · ${saved.origin === "config" ? "From configuration file" : "Saved on this gateway"} · ${saved.credentialSaved ? "Token saved" : "Token required"}`
      : "No saved connection for this provider.";
    $("connection-status").textContent = "";
  }
  $("manage-connections").onclick = async () => {
    if (connectionBusy) return;
    const workspace = state.workspace;
    try {
      const records = await api(`/${workspace}/connections`);
      if (workspace !== state.workspace || !state.data?.canManageConnections)
        return;
      connectionWorkspace = workspace;
      connectionRecords = records;
      $("connection-provider").value = "linear";
      fillConnection();
      $("connections-dialog").showModal();
    } catch (error) {
      report(error);
    }
  };
  $("connection-provider").onchange = fillConnection;
  $("connection-close").onclick = closeConnections;
  $("connections-dialog").onclose = () => {
    $("connection-token").value = "";
    connectionWorkspace = "";
  };
  async function connectionAction(action) {
    if (connectionBusy || !connectionWorkspace) return;
    if (action !== "disable" && !$("connections-form").reportValidity()) return;
    const workspace = connectionWorkspace,
      provider = $("connection-provider").value;
    const saved = connectionRecords.find((c) => c.provider === provider);
    const body = { provider, revision: saved?.revision || 0 };
    if (action !== "disable")
      Object.assign(body, {
        scope: $("connection-scope").value,
        site: $("connection-site").value,
        email: $("connection-email").value,
        token: $("connection-token").value,
      });
    connectionBusy = true;
    for (const control of $("connections-form").querySelectorAll(
      "input,select,button",
    ))
      control.disabled = true;
    $("connection-status").textContent =
      action === "check" ? "Checking read access…" : "Saving…";
    try {
      const result = await api(`/${workspace}/connections/${action}`, body);
      if (workspace !== connectionWorkspace) return;
      if (action !== "check") {
        $("connection-token").value = "";
        connectionRecords = await api(`/${workspace}/connections`);
        if (workspace !== connectionWorkspace) return;
        fillConnection();
        await refresh();
      }
      if (workspace === connectionWorkspace)
        $("connection-status").textContent = result.status;
    } catch (error) {
      if (workspace === connectionWorkspace)
        $("connection-status").textContent = error.message;
    } finally {
      connectionBusy = false;
      for (const control of $("connections-form").querySelectorAll(
        "input,select,button",
      ))
        control.disabled = false;
      $("connection-disable").disabled = !connectionRecords.find(
        (c) => c.provider === $("connection-provider").value,
      )?.enabled;
    }
  }
  $("connections-form").onsubmit = (event) => {
    event.preventDefault();
    connectionAction("save");
  };
  $("connection-check").onclick = () => connectionAction("check");
  $("connection-disable").onclick = () => connectionAction("disable");
  $("start-demo").onclick = async () => {
    $("start-demo").disabled = true;
    $("status").textContent = "Creating demo project…";
    try {
      const w = await api("/demo", {});
      state.workspace = w.id;
      state.task = null;
      $("task-dialog").close();
      await init();
      $("status").textContent = "Demo ready. Open ‘Fix free shipping at $50’ and start the builder.";
    } catch (e) { report(e); }
    finally { $("start-demo").disabled = false; }
  };
  $("workspace-form").onsubmit = async (e) => {
    e.preventDefault();
    try {
      const w = await api("", { name: $("workspace-name").value });
      state.workspace = w.id;
      $("workspace-name").value = "";
      await init();
    } catch (e) {
      report(e);
    }
  };
  $("workspace").onchange = () => {
    closeConnections();
    state.task = null;
    $("task-dialog").close();
    state.workspace = $("workspace").value;
    state.data = null;
    startLive();
    refresh().catch(report);
  };
  $("task-form").onsubmit = async (e) => {
    e.preventDefault();
    try {
      const t = await api("/" + state.workspace + "/tasks", {
        title: $("task-title").value,
        description: $("task-description").value,
      });
      $("task-form").reset();
      await refresh();
      openTask(t);
    } catch (e) {
      report(e);
    }
  };
  $("member-form").onsubmit = async (e) => {
    e.preventDefault();
    try {
      await api("/" + state.workspace + "/members", {
        userId: $("member-id").value,
        role: $("member-role").value,
      });
      await init();
    } catch (e) {
      report(e);
    }
  };
  $("import-form").onsubmit = async (e) => {
    e.preventDefault();
    try {
      const t = await api("/" + state.workspace + "/import", {
        provider: $("connection").value,
        id: $("issue-key").value,
      });
      await refresh();
      openTask(t);
    } catch (e) {
      report(e);
    }
  };
  $("edit-form").onsubmit = (e) => {
    e.preventDefault();
    action("edit", {
      title: $("edit-title").value,
      description: $("edit-description").value,
      assignee: $("edit-assignee").value,
      builder: $("edit-builder").value,
      reviewer: $("edit-reviewer").value,
    });
  };
  for (const b of document.querySelectorAll("[data-action]"))
    b.onclick = () => action(b.dataset.action);
  $("publish").onclick = () => {
    $("publish-text").textContent =
      "Fathom team handoff — " +
      state.task.title +
      "\nState: " +
      state.task.state +
      "\n\n" +
      state.task.handoff +
      "\n\nReview:\n" +
      state.task.review;
    $("publish-preview").hidden = false;
  };
  $("close-dialog").onclick = () => {
    $("task-dialog").close();
    state.task = null;
  };
  $("task-dialog").onclose = () => {
    state.task = null;
  };
  $("download").onclick = async () => {
    try {
      const r = await fetch("/api/v1/board/" + state.workspace + "/archive", {
        headers: headers(),
      });
      if (!r.ok) throw Error("Workspace archive unavailable");
      const url = URL.createObjectURL(await r.blob());
      const a = document.createElement("a");
      a.href = url;
      a.download = "workspace.tar.gz";
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    } catch (e) {
      report(e);
    }
  };
  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("/sw.js").catch((error) => {
      console.warn("SW registration failed:", error);
    });
  }
  init().catch(report);
  setInterval(() => {
    if (!document.hidden && !liveConnected) refresh().catch(report);
  }, 30000);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) {
      refresh().catch(report);
      if (!liveConnected) startLive();
    }
  });
  window.addEventListener("pagehide", stopLive);
  window.addEventListener("pageshow", () => {
    if (!liveController) startLive();
  });
})();
