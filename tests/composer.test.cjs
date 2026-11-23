const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const test = require("node:test");
const { runInNewContext } = require("node:vm");

const script = readFileSync(join(__dirname, "../web/ui.js"), "utf8");

test("composer shortcut activates Send without changing plain Enter", () => {
  const handlers = {};
  let clicks = 0;
  const send = { disabled: false, click: () => clicks++ };
  const prompt = { addEventListener: (event, handler) => handlers[event] = handler };
  runInNewContext(script, {
    document: {
      querySelector: selector => ({ "#prompt": prompt, "#send": send })[selector] ?? null,
      body: { addEventListener() {} },
      addEventListener() {},
    },
  });
  function press(overrides = {}) {
    let prevented = false;
    handlers.keydown({ key: "Enter", preventDefault: () => { prevented = true; }, ...overrides });
    return prevented;
  }

  assert.equal(press(), false);
  assert.equal(press({ shiftKey: true }), false);
  assert.equal(press({ ctrlKey: true, isComposing: true }), false);
  assert.equal(clicks, 0);
  assert.equal(press({ ctrlKey: true }), true);
  assert.equal(press({ metaKey: true }), true);
  assert.equal(press({ ctrlKey: true, repeat: true }), true);
  send.disabled = true;
  assert.equal(press({ ctrlKey: true }), true);
  assert.equal(clicks, 2);
});

function composerHarness(text) {
  const handlers = {};
  const posts = [];
  const element = extra => ({
    addEventListener() {}, querySelector: () => null, querySelectorAll: () => [],
    setAttribute() {}, matches: () => false, closest: () => null, dataset: {}, ...extra,
  });
  const notice = element({ textContent: "" });
  const prompt = element({ value: text, focus() {} });
  const composer = element({ elements: { session: { value: "session" } } });
  const controls = element({ open: false, showModal() { this.open = true; } });
  const nodes = {
    "#prompt": prompt, "#prompt-form": composer, "#notice": notice, "#controls": controls,
    "#feed": element(), "#scroll-feed": element(), "#send": element(), "#interrupt": element(),
    "#attachments": element({ files: [] }), "#controls-body": element(), "#palette": element({ hidden: true }),
  };
  runInNewContext(script, {
    document: {
      querySelector: selector => nodes[selector] ?? null, querySelectorAll: () => [],
      body: { addEventListener: (name, handler) => { (handlers[name] ??= []).push(handler); } },
      addEventListener() {},
    },
    location: { pathname: "/worker/w" },
    sessionStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    localStorage: { getItem: () => null, setItem() {} },
    htmx: { process() {}, trigger() {}, ajax: (method, url, options) => { posts.push({ method, url, ...options }); return Promise.resolve(); } },
    setTimeout, clearTimeout, setInterval, clearInterval,
  });
  const send = () => {
    let prevented = false;
    for (const handler of handlers["htmx:beforeRequest"]) {
      handler({ detail: { elt: composer, target: notice, requestConfig: {} }, preventDefault: () => { prevented = true; }, get defaultPrevented() { return prevented; } });
    }
    return prevented;
  };
  return { controls, notice, posts, prompt, send };
}

test("sending a direct command runs it without opening the controls", () => {
  for (const [text, action, value] of [["/compact", "compact", ""], ["/compact keep the API notes", "compact", "keep the API notes"], ["/retry", "retry", ""], ["/new", "new", ""], ["/reload", "reload", ""]]) {
    const h = composerHarness(text);
    assert.equal(h.send(), true, text);
    assert.equal(h.posts.length, 1, text);
    assert.equal(h.posts[0].values.action, action);
    assert.equal(h.posts[0].values.value, value);
    assert.equal(h.posts[0].values.inline, "1");
    assert.equal(h.controls.open, false, text);
    assert.equal(h.prompt.value, "");
  }
});

test("a command with choices opens the controls, and other text is still sent", () => {
  const model = composerHarness("/model");
  assert.equal(model.send(), true);
  assert.equal(model.controls.open, true);
  for (const text of ["/retry this differently", "/unknown", "hello"]) {
    const h = composerHarness(text);
    assert.equal(h.send(), false, text);
    assert.equal(h.posts.length, 0, text);
    assert.equal(h.prompt.value, text);
  }
});

test("a failed compact puts its instructions back", async () => {
  const h = composerHarness("/compact keep the API notes");
  h.send();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(h.prompt.value, "/compact keep the API notes");
});

function toastHarness({ saved = null, narrow = false } = {}) {
  const timers = [];
  const element = (tag = "div") => {
    const el = {
      tag, children: [], dataset: {}, listeners: {}, textContent: "", hidden: false, open: false,
      get lastElementChild() { return this.children.at(-1); },
      append(...items) { for (const item of items) { item.parent = this; this.children.push(item); } },
      remove() { this.parent?.children.splice(this.parent.children.indexOf(this), 1); },
      addEventListener(name, handler) { this.listeners[name] = handler; },
      querySelector: selector => selector === "button" ? el.children.flatMap(c => c.children).find(c => c.tag === "button") ?? null : null,
      querySelectorAll: () => [], setAttribute() {}, matches: () => false, closest: () => null, focus() {},
    };
    return el;
  };
  const summary = element("summary");
  const progress = element("details");
  progress.querySelector = () => summary;
  const nodes = {
    "#toast-list": element(), "#progress": progress, "#progress-list": element(), "#progress-live": element(),
    "#progress-count": element(), "#progress-label": element(), "#notice": element(), "#scroll-feed": element(),
  };
  const context = runInNewContext(`${script}; ({ toast, setProgress })`, {
    document: {
      querySelector: selector => nodes[selector] ?? null, querySelectorAll: () => [],
      createElement: element, body: { addEventListener() {} }, addEventListener() {},
    },
    localStorage: { getItem: () => saved, setItem() {} },
    matchMedia: () => ({ matches: narrow }),
    MutationObserver: class { observe() {} },
    setTimeout: fn => timers.push(fn),
  });
  return { ...context, timers, progress, list: nodes["#toast-list"].children, running: nodes["#progress-list"].children, label: nodes["#progress-label"] };
}

test("errors stay until dismissed while other toasts expire", () => {
  const h = toastHarness();
  h.toast("Saved.");
  h.toast("It broke", true);
  assert.equal(h.timers.length, 1);
  h.timers[0]();
  assert.deepEqual(h.list.map(item => item.dataset.text), ["It broke"]);
  h.list[0].children[1].listeners.click();
  assert.equal(h.list.length, 0);
});

test("a repeated toast moves to the newest position", () => {
  const h = toastHarness();
  h.toast("one", true);
  h.toast("two", true);
  h.toast("one", true);
  assert.deepEqual(h.list.map(item => item.dataset.text), ["two", "one"]);
});

test("progress stays in start order and cannot be dismissed", () => {
  const h = toastHarness();
  assert.equal(h.progress.open, true);
  h.setProgress("compact", "Compacting…");
  h.setProgress("connection", "Reconnecting…");
  assert.deepEqual(h.running.map(item => item.dataset.key), ["compact", "connection"]);
  assert.equal(h.running.some(item => item.children.length), false);
  h.setProgress("compact");
  h.setProgress("connection");
  assert.equal(h.progress.hidden, true);
});

test("progress starts collapsed on phones and shows the newest item", () => {
  const h = toastHarness({ narrow: true });
  assert.equal(h.progress.open, false);
  h.setProgress("compact", "Compacting…");
  h.setProgress("connection", "Reconnecting…");
  assert.equal(h.label.textContent, "Reconnecting…");
  assert.equal(toastHarness({ narrow: true, saved: "true" }).progress.open, true);
});
