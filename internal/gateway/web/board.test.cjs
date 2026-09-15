const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

class Element {
  constructor() {
    this.children = [];
    this.value = "";
    this.style = {};
    this.dataset = {};
    this.selectors = new Map();
  }
  append(...children) {
    this.children.push(...children);
  }
  replaceChildren(...children) {
    this.children = children;
  }
  querySelector(selector) {
    if (!this.selectors.has(selector))
      this.selectors.set(selector, new Element());
    return this.selectors.get(selector);
  }
  querySelectorAll() {
    return [];
  }
  showModal() {
    this.open = true;
  }
  close() {
    this.open = false;
  }
}
const tick = () => new Promise((resolve) => setImmediate(resolve));
function task(state = "running", revision = 2) {
  return {
    id: "task",
    state,
    revision,
    title: "Demo",
    description: "Brief",
    assignee: "",
    builder: "",
    reviewer: "",
    handoff: "",
    review: "",
  };
}
function snapshot(t, activity = []) {
  return {
    role: "member",
    tasks: [t],
    activity,
    members: [{ userId: "alice", role: "member" }],
    connections: [],
  };
}
async function setup() {
  const elements = new Map();
  const el = (id) => {
    if (!elements.has(id)) elements.set(id, new Element());
    return elements.get(id);
  };
  const env = {
    data: snapshot(task()),
    streams: [],
    timer: null,
    snapshots: 0,
    denied: false,
  };
  const context = {
    document: {
      getElementById: el,
      createElement: () => new Element(),
      createTextNode: (text) => ({ textContent: text }),
      querySelectorAll: () => [],
      addEventListener() {},
    },
    window: { addEventListener() {} },
    localStorage: { getItem: () => "token" },
    navigator: {},
    console,
    URL,
    AbortController,
    TextDecoder,
    setInterval() {},
    setTimeout: (fn) => {
      env.timer = fn;
      return 1;
    },
    clearTimeout: () => {
      env.timer = null;
    },
    fetch: async (url, options = {}) => {
      if (url === "/api/v1/board")
        return { ok: true, json: async () => ({ models: [], workspaces: [] }) };
      if (url.endsWith("/events")) {
        if (env.denied) return { ok: false, status: 401 };
        let controller;
        const body = new ReadableStream({
          start(c) {
            controller = c;
          },
        });
        env.streams.push(controller);
        return { ok: true, status: 200, body };
      }
      if (options.method === "POST") {
        env.posted = JSON.parse(options.body);
        return new Promise((resolve) => {
          env.resolvePost = resolve;
        });
      }
      if (url.endsWith("/evidence")) return { ok: true, json: async () => [] };
      env.snapshots++;
      return { ok: true, json: async () => env.data };
    },
  };
  vm.createContext(context);
  const source = fs.readFileSync(path.join(__dirname, "board.js"), "utf8");
  vm.runInContext(
    source.replace(
      /\}\)\(\);\s*$/,
      "globalThis.board={state,startLive,stopLive,refresh,action,openTask};})();",
    ),
    context,
  );
  await tick();
  env.board = context.board;
  env.el = el;
  env.board.state.workspace = "workspace";
  env.board.state.data = env.data;
  env.board.openTask(env.data.tasks[0]);
  await tick();
  env.emit = (text = "event: changed\ndata: {}\n\n") =>
    env.streams.at(-1).enqueue(new TextEncoder().encode(text));
  return env;
}
test("live comments preserve a draft and reconnect recovers completion", async () => {
  const e = await setup();
  e.el("task-comment").value = "Draft in progress";
  e.board.startLive();
  await tick();
  e.data = snapshot(task(), [
    {
      id: 1,
      taskId: "task",
      actor: "bob",
      kind: "comment",
      at: new Date().toISOString(),
      text: "Hello while running",
    },
  ]);
  e.emit("event: cha");
  e.emit("nged\ndata: {}\n\n");
  await tick();
  assert.equal(e.el("task-comment").value, "Draft in progress");
  assert.equal(e.el("task-activity").children.length, 1);
  assert.equal(
    e.el("task-activity").children[0].children[1].textContent,
    "Hello while running",
  );
  e.emit();
  await tick();
  assert.equal(e.el("task-activity").children.length, 1);
  e.streams.at(-1).close();
  await tick();
  assert.equal(typeof e.timer, "function");
  const finished = task("review", 3);
  finished.handoff = "Done";
  e.data = snapshot(finished, e.data.activity);
  e.timer();
  await tick();
  e.emit();
  await tick();
  assert.equal(e.board.state.task.state, "review");
  assert.equal(e.el("task-comment").value, "Draft in progress");
  assert.equal(e.el("live-status").textContent, "Live updates connected");
  e.board.stopLive();
});
test("comment POST and agent completion cannot erase the next draft", async () => {
  const e = await setup();
  e.el("task-comment").value = "Send this";
  const request = e.board.action("comment");
  await tick();
  assert.equal(e.posted.text, "Send this");
  e.el("task-comment").value = "Next draft";
  const finished = task("review", 3);
  finished.handoff = "Done";
  e.data = snapshot(finished, [
    {
      id: 1,
      taskId: "task",
      actor: "alice",
      kind: "comment",
      text: "Send this",
      at: new Date().toISOString(),
    },
  ]);
  await e.board.refresh();
  e.resolvePost({ ok: true, json: async () => ({ status: "Comment added." }) });
  await request;
  assert.equal(e.el("task-comment").value, "Next draft");
  assert.equal(e.board.state.task.state, "review");
  assert.equal(e.el("task-activity").children.length, 1);
});
test("commenting keeps unsaved task edits and clears only submitted text", async () => {
  const e = await setup();
  e.board.openTask(task("review", 3));
  e.el("edit-title").value = "Unsaved title";
  e.el("task-comment").value = "Discussion";
  const request = e.board.action("comment");
  await tick();
  e.resolvePost({ ok: true, json: async () => ({ status: "Comment added." }) });
  await request;
  assert.equal(e.el("edit-title").value, "Unsaved title");
  assert.equal(e.el("task-comment").value, "");
});
test("expired access ends updates without retrying or displaying the workspace", async () => {
  const e = await setup();
  e.denied = true;
  e.board.startLive();
  await tick();
  assert.equal(e.timer, null);
  assert.equal(e.board.state.workspace, "");
  assert.equal(e.el("workspace-content").hidden, true);
  assert.equal(e.el("task-dialog").open, false);
});
