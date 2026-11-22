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
