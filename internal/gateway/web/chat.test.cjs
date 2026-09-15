const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

// Minimal DOM adapter: exercise the production send/render/event functions
// without a bundler or a browser dependency. Server response timing is explicit.
class Element {
  constructor() { this.children = []; this.dataset = {}; this.style = {}; this.classList = { toggle() {}, add() {}, remove() {} }; this.value = ''; }
  appendChild(child) { child.parent = this; this.children.push(child); return child; }
  remove() { if (this.parent) this.parent.children = this.parent.children.filter(c => c !== this); }
  set innerHTML(value) { this.children = []; }
  addEventListener() {}
  querySelector() { return new Element(); }
  focus() {}
}
function setup() {
  const elements = new Map();
  const element = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  let resolvePost, posted;
  const context = {
    document: { getElementById: element, querySelectorAll: () => [], createElement: () => new Element(), createTextNode: text => ({ textContent: text }), body: new Element() },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    location: { search: '', pathname: '/', hash: '' }, navigator: { userAgent: '' }, window: { innerHeight: 1000, addEventListener() {} },
    URLSearchParams, AbortController, TextDecoder, console,
    setTimeout() {}, setInterval() {}, clearInterval() {},
    fetch: (url, options) => {
      if (url.endsWith('/stream')) return Promise.resolve({ ok: true, body: { getReader: () => ({ read: async () => ({ done: true }) }) } });
      if (url.endsWith('/messages')) {
        posted = JSON.parse(options.body);
        return new Promise(resolve => { resolvePost = resolve; });
      }
      return Promise.resolve({ ok: true, json: async () => ({ thread: { title: 'Test' }, threads: [] }) });
    },
  };
  vm.createContext(context);
  const source = fs.readFileSync(path.join(__dirname, 'chat.js'), 'utf8');
  vm.runInContext(source.replace(/\}\)\(\);\s*$/, 'globalThis.chat = {state, send, renderMessage, handleStreamEvent, switchThread}; })();'), context);
  const chat = context.chat;
  chat.state.token = 'token'; chat.state.currentThreadId = 'thread';
  return { chat, history: element('history'), get posted() { return posted; }, complete(user, agent) { resolvePost({ ok: true, json: async () => ({ user_message: user, agent_message: agent }) }); } };
}
for (const order of ['sse-first', 'post-first']) {
  test(`one user bubble and thinking indicator: ${order}`, async () => {
    const env = setup(); const { chat, history } = env;
    const sending = chat.send('hello');
    const user = { id: 'server-user', thread_id: 'thread', role: 'user', content: 'hello', metadata: { clientMessageId: env.posted.clientMessageId } };
    const agent = { id: 'server-agent', thread_id: 'thread', role: 'agent', content: 'reply' };
    const event = (name, message) => chat.handleStreamEvent({ event: name, data: { message } });
    assert.equal(history.children.filter(c => c.className === 'turn-user').length, 1);
    event('agent_thinking'); event('agent_thinking');
    assert.equal(history.children.filter(c => c.dataset.spinner).length, 1);
    if (order === 'sse-first') { event('user_message', user); event('agent_done', agent); }
    env.complete(user, agent); await sending;
    event('user_message', user); event('agent_done', agent);
    assert.equal(history.children.filter(c => c.className === 'turn-user').length, 1);
    assert.equal(history.children.filter(c => c.className === 'turn-agent').length, 1);
    assert.equal(history.children.filter(c => c.dataset.spinner).length, 0);
    assert.equal(chat.state.pendingUsers.size, 0);
    assert.equal(chat.state.rendered.size, 2);
  });
}
test('identical text from another device is a separate message', async () => {
  const env = setup(); const { chat, history } = env; const sending = chat.send('same');
  chat.renderMessage({ id: 'remote', thread_id: 'thread', role: 'user', content: 'same' });
  const user = { id: 'own', thread_id: 'thread', role: 'user', content: 'same', metadata: { clientMessageId: env.posted.clientMessageId } };
  env.complete(user, { id: 'reply', thread_id: 'thread', role: 'agent', content: 'ok' }); await sending;
  assert.equal(history.children.filter(c => c.className === 'turn-user').length, 2);
});
test('late response cannot populate a different thread', async () => {
  const env = setup(); const { chat, history } = env; const sending = chat.send('old');
  await chat.switchThread('other');
  env.complete({ id: 'old-user', thread_id: 'thread', role: 'user', content: 'old' }, { id: 'old-agent', thread_id: 'thread', role: 'agent', content: 'old' });
  await sending;
  assert.equal(history.children.length, 0);
});
