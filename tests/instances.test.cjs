const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const test = require("node:test");
const { runInNewContext } = require("node:vm");

const script = readFileSync(join(__dirname, "../web/ui.js"), "utf8");

function harness(promptReply) {
  const handlers = {};
  const refreshes = [];
  const notice = {
    textContent: "",
    append(child) { this.textContent += child.textContent; },
  };
  const instances = {
    addEventListener() {}, querySelectorAll: () => [], contains: () => false,
    matches: () => false, closest: () => null,
  };
  const body = {
    addEventListener(name, handler) { (handlers[name] ??= []).push(handler); },
  };
  const document = {
    body, hidden: false, activeElement: body, addEventListener() {},
    createElement: () => ({ className: "", textContent: "" }),
    querySelector: selector => ({ "#instance-list": instances, "#lifecycle-notice": notice })[selector] ?? null,
  };
  runInNewContext(script, {
    document,
    location: { pathname: "/worker/w" },
    EventSource: class { addEventListener() {} close() {} },
    htmx: { trigger: (_, name) => refreshes.push(name) },
    prompt: () => promptReply,
    localStorage: { getItem: () => null, setItem() {} },
    sessionStorage: { getItem: () => null, setItem() {} },
  });
  const fire = (name, detail) => {
    const event = { detail, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; } };
    for (const handler of handlers[name] ?? []) handler(event);
    return event;
  };
  const card = () => {
    const button = { textContent: "Restart", dataset: { busyLabel: "Restarting…" } };
    const form = { querySelector: () => button };
    const elt = { closest: selector => selector === ".instance-card form" ? form : selector === ".instance-card" ? form : null, matches: () => false };
    return { button, detail: { elt, xhr: {}, requestConfig: {}, successful: true } };
  };
  const list = type => ({ elt: instances, target: instances, xhr: {}, requestConfig: { triggeringEvent: { type } }, serverResponse: "<div></div>", shouldSwap: true });
  return { fire, card, list, notice, refreshes };
}

test("each card shows its own progress and the list refreshes after the last one", () => {
  const { fire, card, refreshes } = harness();
  const first = card();
  const second = card();
  fire("htmx:beforeRequest", first.detail);
  fire("htmx:beforeRequest", second.detail);
  assert.equal(first.button.textContent, "Restarting…");
  assert.equal(second.button.textContent, "Restarting…");

  fire("htmx:afterRequest", first.detail);
  assert.equal(first.button.textContent, "Restart");
  assert.equal(second.button.textContent, "Restarting…");
  assert.equal(refreshes.length, 0);

  fire("htmx:afterRequest", second.detail);
  assert.deepEqual(refreshes, ["instanceListRefresh"]);
});

test("a list response can't replace cards while another restart is running", () => {
  const { fire, card, list } = harness();
  const first = card();
  const second = card();
  fire("htmx:beforeRequest", first.detail);
  fire("htmx:beforeRequest", second.detail);
  fire("htmx:afterRequest", first.detail);

  const response = list("instanceListRefresh");
  fire("htmx:beforeSwap", response);
  assert.equal(response.shouldSwap, false);
});

test("htmx reporting a detached card twice doesn't end another card's request", () => {
  const { fire, card, list } = harness();
  const first = card();
  const second = card();
  fire("htmx:beforeRequest", first.detail);
  fire("htmx:beforeRequest", second.detail);
  fire("htmx:afterRequest", first.detail);
  fire("htmx:afterRequest", first.detail);

  const response = list("every");
  fire("htmx:beforeSwap", response);
  assert.equal(response.shouldSwap, false);
});

test("a restart finishing during a list load refreshes again afterward", () => {
  const { fire, card, list, refreshes } = harness();
  const restart = card();
  fire("htmx:beforeRequest", restart.detail);
  const loading = list("instanceListRefresh");
  fire("htmx:beforeRequest", loading);
  fire("htmx:afterRequest", restart.detail);
  assert.equal(refreshes.length, 0);

  fire("htmx:afterRequest", loading);
  assert.deepEqual(refreshes, ["instanceListRefresh"]);
});

test("a new click keeps errors from restarts in the same batch", () => {
  const { fire, card, notice } = harness();
  const first = card();
  const second = card();
  fire("htmx:beforeRequest", first.detail);
  notice.textContent = "First failed.";
  fire("htmx:beforeRequest", second.detail);
  assert.equal(notice.textContent, "First failed.");

  fire("htmx:sendError", second.detail);
  assert.match(notice.textContent, /^First failed\..*No confirmation/);
  fire("htmx:afterRequest", first.detail);
  fire("htmx:afterRequest", second.detail);

  fire("htmx:beforeRequest", card().detail);
  assert.equal(notice.textContent, "");
});

test("renaming from a card sends the new name and skips a cancelled or unchanged one", () => {
  const confirm = reply => {
    const { fire } = harness(reply);
    const value = { value: "Old" };
    const elt = { matches: selector => selector === "[data-rename]", dataset: { rename: "Old" }, elements: { value } };
    return { blocked: fire("htmx:confirm", { elt }).defaultPrevented, sent: value.value };
  };
  assert.deepEqual({ ...confirm("New") }, { blocked: false, sent: "New" });
  assert.deepEqual({ ...confirm("") }, { blocked: false, sent: "" });
  assert.equal(confirm(null).blocked, true);
  assert.equal(confirm(" Old ").blocked, true);
});
