// Settings page logic. Reads GET /api/v1/settings, renders the controls,
// and PATCHes a partial diff on save. When the server says the account
// can't edit (team/enterprise viewer), every control is disabled and a
// banner explains why.
//
// Token comes from the same localStorage key the chat UI uses, so pairing
// in the chat view carries over here with no extra sign-in.
(function () {
  "use strict";

  const TOKEN_KEY = "fantazm_device_token";
  const token = localStorage.getItem(TOKEN_KEY);

  const $ = (id) => document.getElementById(id);
  const els = {
    status: $("status"),
    form: $("form"),
    banner: $("readonly-banner"),
    modePill: $("mode-pill"),
    shell: $("feat-shell"),
    subagents: $("feat-subagents"),
    swarm: $("feat-swarm"),
    claudecode: $("feat-claudecode"),
    websearch: $("feat-websearch"),
    modelDefault: $("model-default"),
    takeoverEnabled: $("takeover-enabled"),
    takeoverProvider: $("takeover-provider"),
    network: $("pol-network"),
    domains: $("pol-domains"),
    fs: $("pol-fs"),
    allowPaths: $("pol-allowpaths"),
    denyPaths: $("pol-denypaths"),
    secrets: $("pol-secrets"),
    saveBtn: $("save-btn"),
    saveMsg: $("save-msg"),
    saveBar: $("save-bar"),
  };

  // The last-loaded server state, used to compute the PATCH diff on save.
  let snapshot = null;

  const authHeaders = (extra) =>
    Object.assign({ Authorization: "Bearer " + token }, extra || {});

  const linesToList = (s) =>
    s.split("\n").map((x) => x.trim()).filter((x) => x.length > 0);
  const listToLines = (arr) => (arr || []).join("\n");

  async function load() {
    if (!token) {
      els.status.textContent =
        "No device token found. Open the chat view and pair this device first.";
      return;
    }
    try {
      const res = await fetch("api/v1/settings", { headers: authHeaders() });
      if (res.status === 401) {
        els.status.textContent =
          "Session expired. Re-pair from the chat view.";
        return;
      }
      if (!res.ok) {
        els.status.textContent = "Failed to load settings (" + res.status + ").";
        return;
      }
      snapshot = await res.json();
      render(snapshot);
    } catch (e) {
      els.status.textContent = "Network error loading settings.";
    }
  }

  function render(s) {
    els.status.hidden = true;
    els.form.hidden = false;

    els.modePill.textContent = (s.mode || "personal") + " mode";

    els.shell.checked = !!(s.features && s.features.shell);
    els.subagents.checked = !!(s.features && s.features.subAgents);
    els.swarm.checked = !!(s.features && s.features.swarm);
    els.claudecode.checked = !!(s.features && s.features.claudeCode);
    els.websearch.checked = !!(s.features && s.features.webSearch);

    // Takeover: toggle + provider select.
    const tk = s.takeover || {};
    els.takeoverEnabled.checked = !!tk.enabled;
    els.takeoverProvider.innerHTML = "";
    (tk.providers || ["claude"]).forEach((pr) => {
      const o = document.createElement("option");
      o.value = pr;
      o.textContent = pr;
      if (pr === (tk.provider || "claude")) o.selected = true;
      els.takeoverProvider.appendChild(o);
    });

    // Model dropdown.
    els.modelDefault.innerHTML = "";
    const models = (s.models && s.models.available) || [];
    const def = (s.models && s.models.default) || "";
    if (models.length === 0 && def) models.push(def);
    if (models.length === 0) {
      const o = document.createElement("option");
      o.value = "";
      o.textContent = "(no models registered)";
      els.modelDefault.appendChild(o);
    }
    models.forEach((m) => {
      const o = document.createElement("option");
      o.value = m;
      o.textContent = m;
      if (m === def) o.selected = true;
      els.modelDefault.appendChild(o);
    });

    const p = s.policy || {};
    els.network.value = p.network || "deny";
    els.domains.value = listToLines(p.allowedDomains);
    els.fs.value = p.filesystem || "read-only";
    els.allowPaths.value = listToLines(p.allowedPaths);
    els.denyPaths.value = listToLines(p.deniedPaths);
    els.secrets.value = p.secrets || "isolated";

    setEditable(!!s.editable);
  }

  function setEditable(editable) {
    els.banner.hidden = editable;
    const controls = [
      els.shell, els.subagents, els.swarm, els.claudecode, els.websearch,
      els.modelDefault, els.takeoverEnabled, els.takeoverProvider,
      els.network, els.domains, els.fs, els.allowPaths, els.denyPaths,
      els.secrets,
    ];
    controls.forEach((c) => (c.disabled = !editable));
    els.saveBar.style.display = editable ? "" : "none";
  }

  // Build a PATCH body containing only fields that changed from snapshot.
  function buildPatch() {
    const s = snapshot;
    const patch = {};

    const features = {};
    if (els.shell.checked !== !!s.features.shell) features.shell = els.shell.checked;
    if (els.subagents.checked !== !!s.features.subAgents) features.subAgents = els.subagents.checked;
    if (els.swarm.checked !== !!s.features.swarm) features.swarm = els.swarm.checked;
    if (els.claudecode.checked !== !!s.features.claudeCode) features.claudeCode = els.claudecode.checked;
    if (els.websearch.checked !== !!s.features.webSearch) features.webSearch = els.websearch.checked;
    if (Object.keys(features).length) patch.features = features;

    if (els.modelDefault.value && els.modelDefault.value !== (s.models.default || "")) {
      patch.models = { default: els.modelDefault.value };
    }

    const tk = s.takeover || {};
    const takeover = {};
    if (els.takeoverEnabled.checked !== !!tk.enabled) takeover.enabled = els.takeoverEnabled.checked;
    if (els.takeoverProvider.value && els.takeoverProvider.value !== (tk.provider || "claude"))
      takeover.provider = els.takeoverProvider.value;
    if (Object.keys(takeover).length) patch.takeover = takeover;

    const p = s.policy || {};
    const policy = {};
    if (els.network.value !== p.network) policy.network = els.network.value;
    if (els.fs.value !== p.filesystem) policy.filesystem = els.fs.value;
    if (els.secrets.value !== p.secrets) policy.secrets = els.secrets.value;

    const newDomains = linesToList(els.domains.value);
    if (listToLines(newDomains) !== listToLines(p.allowedDomains))
      policy.allowedDomains = newDomains;
    const newAllow = linesToList(els.allowPaths.value);
    if (listToLines(newAllow) !== listToLines(p.allowedPaths))
      policy.allowedPaths = newAllow;
    const newDeny = linesToList(els.denyPaths.value);
    if (listToLines(newDeny) !== listToLines(p.deniedPaths))
      policy.deniedPaths = newDeny;
    if (Object.keys(policy).length) patch.policy = policy;

    return patch;
  }

  async function save() {
    const patch = buildPatch();
    if (Object.keys(patch).length === 0) {
      showMsg("Nothing changed.", "");
      return;
    }
    els.saveBtn.disabled = true;
    showMsg("Saving…", "");
    try {
      const res = await fetch("api/v1/settings", {
        method: "PATCH",
        headers: authHeaders({ "Content-Type": "application/json" }),
        body: JSON.stringify(patch),
      });
      if (res.status === 403) {
        showMsg("Settings are managed by your administrator.", "err");
        setEditable(false);
        els.saveBtn.disabled = false;
        return;
      }
      if (!res.ok) {
        let msg = "Save failed (" + res.status + ").";
        try { const j = await res.json(); if (j.error) msg = j.error; } catch (e) {}
        showMsg(msg, "err");
        els.saveBtn.disabled = false;
        return;
      }
      const updated = await res.json();
      snapshot = updated;
      render(updated);
      els.saveBtn.disabled = false;
      if (updated.restartRequired) {
        showMsg("Saved. Restart the gateway for model / sub-agent changes to take effect.", "ok");
      } else {
        showMsg("Saved. Changes are live.", "ok");
      }
    } catch (e) {
      showMsg("Network error while saving.", "err");
      els.saveBtn.disabled = false;
    }
  }

  function showMsg(text, kind) {
    els.saveMsg.textContent = text;
    els.saveMsg.className = "save-msg" + (kind ? " " + kind : "");
  }

  els.saveBtn.addEventListener("click", save);
  // Clear the "saved" message once the user edits again.
  els.form.addEventListener("input", () => {
    if (els.saveMsg.textContent) showMsg("", "");
  });

  load();
})();
