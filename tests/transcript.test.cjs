const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const test = require("node:test");
const { runInNewContext } = require("node:vm");

const script = readFileSync(join(__dirname, "../web/ui.js"), "utf8");

test("a result-only refresh keeps the live command header and its position", () => {
  let rows;
  function row(id, { live = false, resultOnly = false, command, status, output } = {}) {
    const attributes = new Set();
    if (live) attributes.add("data-live");
    if (resultOnly) attributes.add("data-result-only");
    const parts = Object.fromEntries(Object.entries({ status, output }).map(([key, text]) => [`.execution-${key}`, { text }]));
    const node = {
      dataset: { entry: id }, command,
      matches: selector => selector === ".execution:not([data-result-only])" && id.startsWith("tool:") && !resultOnly,
      hasAttribute: name => attributes.has(name),
      removeAttribute: name => attributes.delete(name),
      querySelectorAll: () => [],
      querySelector: selector => ({ ...parts[selector], replaceWith: next => parts[selector] = next }),
      replaceWith: next => rows.splice(rows.indexOf(node), 1, next),
      remove: () => rows.splice(rows.indexOf(node), 1),
    };
    return node;
  }
  const intro = row("entry:intro");
  const call = row("tool:1", { live: true, command: "printf hello", status: "Running" });
  const result = row("tool:1", { resultOnly: true, status: "Done", output: "hello" });
  const after = row("entry:after");
  rows = [intro, call];
  const handlers = {};
  const feed = { querySelectorAll: selector => rows.filter(node => !selector.includes(":not") || !node.hasAttribute("data-live")) };
  const context = {
    document: {
      activeElement: { matches: () => false, closest: () => null },
      querySelector: selector => ({
        "#feed": feed,
        "#scroll-feed": { addEventListener() {}, querySelectorAll: selector => selector === "[data-entry]" ? rows.slice() : [], getBoundingClientRect: () => ({ top: 0 }) },
      })[selector] ?? null,
      body: { addEventListener: (event, handler) => handlers[event] = handler },
      addEventListener() {},
    },
    location: { pathname: "/worker/w" },
    EventSource: class { addEventListener() {} close() {} },
  };
  runInNewContext(script, context);
  handlers["htmx:beforeSwap"]({ detail: { target: feed, shouldSwap: true, serverResponse: "new result" } });
  rows = [intro, result, after];
  const seen = runInNewContext("reconcileRows()", context);
  assert.deepEqual(rows, [intro, call, after]);
  assert.equal(seen.get("tool:1").command, "printf hello");
  assert.equal(call.hasAttribute("data-live"), false);
  assert.equal(call.querySelector(".execution-status").text, "Done");
  assert.equal(call.querySelector(".execution-output").text, "hello");
});

test("the transcript follows from the bottom until scrolled up past 5% of its height", () => {
  const listeners = {};
  const handlers = {};
  let top = 0;
  const viewport = {
    clientHeight: 1000, scrollHeight: 3000,
    get scrollTop() { return top; },
    set scrollTop(value) { top = Math.max(0, Math.min(value, this.scrollHeight - this.clientHeight)); },
    addEventListener: (name, handler) => listeners[name] = handler,
    querySelectorAll: () => [], getBoundingClientRect: () => ({ top: 0 }),
  };
  const feed = { querySelectorAll: () => [] };
  const active = { matches: () => false, closest: () => null };
  const context = {
    document: {
      activeElement: active,
      querySelector: selector => ({ "#feed": feed, "#scroll-feed": viewport })[selector] ?? null,
      querySelectorAll: () => [],
      body: { addEventListener: (name, handler) => (handlers[name] ??= []).push(handler) },
      addEventListener() {},
    },
    location: { pathname: "/worker/w" },
    EventSource: class { addEventListener() {} close() {} },
  };
  runInNewContext(script, context);
  runInNewContext("syncQueue = () => {}", context);
  let response = 0;
  const poll = (growth = 200) => {
    const detail = { target: feed, shouldSwap: true, serverResponse: `update ${++response}` };
    handlers["htmx:beforeSwap"].forEach(handler => handler({ detail }));
    viewport.scrollHeight += growth;
    handlers["htmx:afterSwap"].forEach(handler => handler({ detail }));
    listeners.scroll();
  };
  const scrollTo = distance => {
    viewport.scrollTop = viewport.scrollHeight - viewport.clientHeight - distance;
    listeners.scroll();
  };
  const distance = () => viewport.scrollHeight - viewport.clientHeight - viewport.scrollTop;

  assert.equal(distance(), 0, "a new page opens at the latest message");
  poll(400);
  assert.equal(distance(), 0, "a tall message stays followed");
  active.closest = () => ({});
  poll();
  assert.equal(distance(), 0, "focus inside a row does not stop following");
  active.closest = () => null;
  viewport.scrollHeight += 300;
  listeners.load();
  assert.equal(distance(), 0, "a late-loading image stays followed");

  scrollTo(40);
  poll();
  assert.equal(distance(), 0, "a nudge within 5% keeps following");

  scrollTo(60);
  poll();
  assert.equal(distance(), 260, "scrolling up past 5% stops following");
  scrollTo(10);
  poll();
  assert.equal(distance(), 210, "near the bottom is not back at the bottom");

  scrollTo(0);
  poll();
  assert.equal(distance(), 0, "reaching the bottom resumes following");
});

test("a usage-limit wait counts down, then holds at retrying now", () => {
  const span = { textContent: "waiting to retry" };
  const wait = { dataset: { resetsAt: new Date(Date.now() + 3661000).toISOString() }, querySelector: () => span };
  const context = {
    setInterval: () => 1,
    clearInterval() {},
    document: {
      activeElement: { matches: () => false, closest: () => null },
      querySelector: selector => ({
        "#feed": { querySelectorAll: () => [] },
        "#scroll-feed": { addEventListener() {}, querySelectorAll: () => [], getBoundingClientRect: () => ({ top: 0 }) },
        "[data-resets-at]": wait,
      })[selector] ?? null,
      body: { addEventListener() {} },
      addEventListener() {},
    },
    location: { pathname: "/worker/w" },
    EventSource: class { addEventListener() {} close() {} },
  };
  runInNewContext(script, context);
  assert.equal(span.textContent, "retrying in 1h 1m 1s");
  wait.dataset.resetsAt = new Date(Date.now() - 1000).toISOString();
  runInNewContext("showUsageWait()", context);
  assert.equal(span.textContent, "retrying now");
});

test("the image viewer opens, zooms only oversized images, and closes on the background", () => {
  const listeners = {};
  const element = extra => ({
    addEventListener() {}, querySelector: () => null, querySelectorAll: () => [],
    setAttribute() {}, matches: () => false, closest: () => null, dataset: {}, ...extra,
  });
  const classes = new Set();
  const image = element({
    tagName: "IMG", naturalWidth: 3000, naturalHeight: 500,
    classList: {
      remove: (...names) => names.forEach(name => classes.delete(name)),
      toggle(name, on = !classes.has(name)) { on ? classes.add(name) : classes.delete(name); },
    },
    removeAttribute() {},
    getBoundingClientRect: () => ({ left: 0, top: 0, width: 100, height: 100 }),
    matches: selector => selector === ".zoomed, .can-zoom" && (classes.has("zoomed") || classes.has("can-zoom")),
  });
  const viewer = element({
    open: false, clientWidth: 1000, clientHeight: 800, scrollLeft: 0, scrollTop: 0,
    querySelector: () => image, scrollTo() {},
    showModal() { this.open = true; }, close() { this.open = false; },
  });
  image.closest = selector => selector === "#image-viewer" ? viewer : null;
  viewer.closest = image.closest;
  const nodes = { "#image-viewer": viewer, "#feed": element(), "#scroll-feed": element() };
  runInNewContext(script, {
    document: {
      querySelector: selector => nodes[selector] ?? null, querySelectorAll: () => [],
      body: { addEventListener() {} },
      addEventListener: (name, handler) => { (listeners[name] ??= []).push(handler); },
    },
    window: { addEventListener() {} }, localStorage: { getItem: () => null, setItem() {} },
    location: { pathname: "/", search: "" }, requestAnimationFrame() {}, setTimeout() {}, setInterval() {},
    EventSource: class { addEventListener() {} close() {} },
    addEventListener() {}, CSS: { escape: value => value },
  });
  const click = (target, clientX = 10, clientY = 10) => listeners.click.forEach(handler => handler({ target, clientX, clientY }));
  const source = element({ alt: "shot", src: "/image", closest: selector => selector === ".message-image" ? source : null });

  click(source);
  assert.equal(viewer.open, true);
  assert.equal(image.src, "/image");
  image.onload();
  click(image);
  assert.equal(classes.has("zoomed"), true);
  click(image);
  assert.equal(classes.has("zoomed"), false);
  click(viewer, 1005, 10);
  assert.equal(viewer.open, true, "a scrollbar press keeps the viewer open");
  click(viewer);
  assert.equal(viewer.open, false);

  image.naturalWidth = 200;
  click(source);
  image.onload();
  click(image);
  assert.equal(classes.has("zoomed"), false, "an image that fits has nothing to zoom");
});
