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
function setup(options = {}) {
  const elements = new Map();
  const element = id => { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); };
  let resolvePost, posted;
  const context = {
    document: { getElementById: element, querySelectorAll: () => [], createElement: () => new Element(), createTextNode: text => ({ textContent: text }), body: new Element() },
    localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    location: { search: '', pathname: '/', hash: '' }, navigator: { userAgent: '' }, window: { innerHeight: 1000, addEventListener() {} },
    URLSearchParams, AbortController, TextDecoder, console,
    setTimeout: options.setTimeout || (()=>{}), clearTimeout: options.clearTimeout || (()=>{}), setInterval() {}, clearInterval() {},
    fetch: options.fetch || ((url, options) => {
      if (url.endsWith('/stream')) return Promise.resolve({ ok: true, body: { getReader: () => ({ read: async () => ({ done: true }) }) } });
      if (url.endsWith('/messages')) {
        posted = JSON.parse(options.body);
        return new Promise(resolve => { resolvePost = resolve; });
      }
      return Promise.resolve({ ok: true, json: async () => ({ thread: { title: 'Test' }, threads: [] }) });
    }),
  };
  vm.createContext(context);
  const source = fs.readFileSync(path.join(__dirname, 'chat.js'), 'utf8');
  vm.runInContext(source.replace(/\}\)\(\);\s*$/, 'globalThis.chat = {state, send, renderMessage, handleStreamEvent, switchThread, subscribeSSE, teardownSSE}; })();'), context);
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

test('clean SSE EOF reconnects and snapshot recovers missed messages', async()=>{
 let timer,streams=0,snapshots=0;const data={id:'missed',thread_id:'thread',role:'agent',content:'completed while offline'};
 const env=setup({setTimeout:fn=>{timer=fn;return 1},clearTimeout:()=>{timer=null},fetch:async url=>{
 if(url.endsWith('/stream')){streams++;let delivered=false;return {ok:true,body:{getReader:()=>({read:async()=>{if(!delivered){delivered=true;return {value:new TextEncoder().encode('event: ready\ndata: {}\n\n'),done:false}}return {done:true}}})}}}
 snapshots++;return {ok:true,json:async()=>({thread:{title:'Test'},messages:[data],running:false})}
 }});
 env.chat.subscribeSSE('thread');await new Promise(resolve=>setImmediate(resolve));assert.equal(snapshots,1);assert.equal(env.chat.state.rendered.size,1);assert.equal(typeof timer,'function');timer();await new Promise(resolve=>setImmediate(resolve));assert.equal(streams,2);assert.equal(env.chat.state.rendered.size,1);env.chat.teardownSSE();assert.equal(timer,null);
});
test('live deltas reuse thinking bubble and disappear on completion',()=>{const {chat,history}=setup();chat.handleStreamEvent({event:'agent_thinking'});for(const delta of ['hello',' world'])chat.handleStreamEvent({event:'agent_delta',data:{delta}});assert.equal(history.children.filter(c=>c.dataset.spinner).length,1);chat.handleStreamEvent({event:'agent_done',data:{message:{id:'done',thread_id:'thread',role:'agent',content:'hello world'}}});assert.equal(history.children.filter(c=>c.dataset.spinner).length,0);assert.equal(history.children.filter(c=>c.className==='turn-agent').length,1)});
