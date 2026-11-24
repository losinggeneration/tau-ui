const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const test = require("node:test");
const { runInNewContext } = require("node:vm");

const script = readFileSync(join(__dirname, "../web/ui.js"), "utf8");

function element(attributes) {
  return {
    getAttribute: name => attributes[name] ?? null,
    setAttribute(name, value) { attributes[name] = value; },
    attributes,
  };
}

function harness() {
  const handlers = {};
  const calls = [];
  const feed = { ...element({ "hx-get": "/worker/old/feed?session=s" }), firstElementChild: null, lastResponse: "same" };
  const list = element({ "hx-get": "/instances?active=old" });
  const other = element({ "hx-post": "/worker/older/lifecycle" });
  const body = { addEventListener(name, handler) { (handlers[name] ??= []).push(handler); } };
  const location = { pathname: "/worker/old" };
  const document = {
    body, hidden: false, activeElement: body, addEventListener() {},
    createElement: () => ({ className: "", textContent: "" }),
    querySelector: selector => selector === "#feed" ? feed : null,
    querySelectorAll: () => [feed, list, other],
  };
  runInNewContext(script, {
    document, location,
    history: { replaceState(_, __, url) { location.pathname = url; } },
    htmx: { process: () => calls.push("process"), trigger: (_, name) => calls.push(name) },
    localStorage: { getItem: () => null, setItem() {} },
    sessionStorage: { getItem: () => null, setItem() {} },
  });
  const failFeed = headers => {
    const detail = { elt: feed, xhr: { getResponseHeader: name => headers[name] ?? null } };
    for (const handler of handlers["htmx:responseError"]) handler({ detail });
  };
  return { calls, failFeed, feed, list, other, location };
}

test("a replacement moves the page in place", () => {
  const { calls, failFeed, feed, list, other, location } = harness();
  failFeed({ "X-Tau-Replacement": "new" });
  assert.equal(location.pathname, "/worker/new");
  assert.equal(feed.attributes["hx-get"], "/worker/new/feed?session=s");
  assert.equal(list.attributes["hx-get"], "/instances?active=new");
  assert.equal(other.attributes["hx-post"], "/worker/older/lifecycle");
  assert.equal(feed.lastResponse, undefined);
  assert.deepEqual(calls, ["process", "instanceListRefresh", "conversationRefresh"]);
});

test("a restarting instance stays on the page until it's replaced", () => {
  const { calls, failFeed, location } = harness();
  failFeed({ "X-Tau-Instance-State": "restarting" });
  assert.equal(location.pathname, "/worker/old");
  assert.deepEqual(calls, []);
});
