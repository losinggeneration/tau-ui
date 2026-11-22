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
        "#scroll-feed": { querySelectorAll: selector => selector === "[data-entry]" ? rows.slice() : [], getBoundingClientRect: () => ({ top: 0 }) },
      })[selector] ?? null,
      body: { addEventListener: (event, handler) => handlers[event] = handler },
      addEventListener() {},
    },
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
