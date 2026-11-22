"use strict";

const feed = document.querySelector("#feed");
const instances = document.querySelector("#instance-list");
const viewport = document.querySelector("#scroll-feed");
const composer = document.querySelector("#prompt-form");
const promptBox = document.querySelector("#prompt");
const send = document.querySelector("#send");
const attachments = document.querySelector("#attachments");
const attachmentList = document.querySelector("#attachment-list");
const attachmentStatus = document.querySelector("#attachment-status");
const queuePanel = document.querySelector("#queue-panel");
const queueItems = document.querySelector("#queue-items");
const queueEditor = document.querySelector("#queue-editor");
const queueForm = document.querySelector("#queue-edit-form");
const queueNotice = document.querySelector("#queue-edit-notice");
let submittedText;
let submittedFiles;
let openExecutions = new Set();
let previousRows = [];
let scrollAnchor;
let focusedSummary;
let reconnecting = false;
let draftKey;
let sending = false;
let followLatest = false;
let oldHeight;

function syncComposer() {
  if (!composer) return;
  const state = feed.firstElementChild?.dataset;
  const session = composer.elements.session;
  if (!session.value && state?.session) {
    session.value = state.session;
    feed.setAttribute("hx-get", `${location.pathname}/feed?session=${encodeURIComponent(state.session)}`);
    htmx.process(feed);
  }
  if (!draftKey && session.value) {
    draftKey = `tau-draft:${session.value}`;
    try { if (!promptBox.value) promptBox.value = sessionStorage.getItem(draftKey) ?? ""; }
    catch { /* The browser can disable storage. */ }
  }
  const ready = !reconnecting && state?.ready === "true" && state.session === session.value;
  for (const button of composer.querySelectorAll("button[type=submit]")) {
    button.disabled = !ready || sending;
  }
  document.querySelector("#interrupt").disabled = !ready || state.running !== "true";
}

function syncQueue() {
  const state = feed.firstElementChild;
  const count = state.dataset.queueCount;
  const items = state.querySelector("#queue-update");
  queuePanel.hidden = !Number(count);
  document.querySelector("#queue-count").textContent = count;
  if (items.innerHTML === queueItems.innerHTML) return;
  const focusedID = document.activeElement?.dataset.queueEdit;
  queueItems.replaceChildren(items.content.cloneNode(true));
  if (focusedID) queueItems.querySelector(`[data-queue-edit="${CSS.escape(focusedID)}"]`)?.focus({preventScroll: true});
}

queueItems?.addEventListener("click", event => {
  const button = event.target.closest("[data-queue-edit]");
  if (!button) return;
  queueForm.elements.session.value = feed.firstElementChild.dataset.session;
  queueForm.elements.id.value = button.dataset.queueEdit;
  queueForm.elements.text.value = button.closest("li").querySelector(".queue-text").content.textContent;
  queueNotice.textContent = "";
  queueEditor.showModal();
});
document.querySelector("#queue-cancel")?.addEventListener("click", () => queueEditor.close());
queueEditor?.addEventListener("cancel", event => {
  if (queueForm.classList.contains("htmx-request")) event.preventDefault();
});
document.body.addEventListener("queueEditSaved", () => {
  queueEditor.close();
  queuePanel.querySelector("summary").focus({preventScroll: true});
});

function saveDraft() {
  if (!draftKey) return;
  try {
    if (promptBox.value) sessionStorage.setItem(draftKey, promptBox.value);
    else sessionStorage.removeItem(draftKey);
  } catch { /* The draft remains on this page. */ }
}

promptBox?.addEventListener("input", saveDraft);
promptBox?.addEventListener("keydown", event => {
  if (paletteKey(event)) return;
  if (event.key !== "Enter" || !(event.ctrlKey || event.metaKey) ||
      event.shiftKey || event.altKey || event.isComposing) return;
  event.preventDefault();
  if (!event.repeat && !sending && !send.disabled) send.click();
});

function setAttachments(files) {
  const transfer = new DataTransfer();
  for (const file of files) transfer.items.add(file);
  attachments.files = transfer.files;
  renderAttachments();
}

function renderAttachments() {
  attachmentList.replaceChildren();
  const files = [...attachments.files];
  files.forEach((file, index) => {
    const row = document.createElement("li");
    const name = document.createElement("span");
    name.textContent = file.name;
    name.title = file.name;
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "quiet";
    remove.textContent = "Remove";
    remove.setAttribute("aria-label", `Remove ${file.name}`);
    remove.addEventListener("click", () => {
      setAttachments(files.filter((_, i) => i !== index));
      const buttons = attachmentList.querySelectorAll("button");
      (buttons[Math.min(index, buttons.length - 1)] ?? attachments).focus();
    });
    row.append(name, remove);
    attachmentList.append(row);
  });
  attachmentStatus.textContent = "";
}

attachments?.addEventListener("change", renderAttachments);
promptBox?.addEventListener("paste", event => {
  const images = [...(event.clipboardData?.files ?? [])].filter(file => file.type.startsWith("image/"));
  if (!images.length || sending) return;
  setAttachments([...attachments.files, ...images]);
  if (!event.clipboardData.types.includes("text/plain")) event.preventDefault();
});

function bottom() {
  if (viewport) viewport.scrollTop = viewport.scrollHeight;
}

function reconcileRows() {
  const latest = new Map(Array.from(feed.querySelectorAll("[data-entry]"), row => [row.dataset.entry, row]));
  const previousCalls = new Map(previousRows.filter(row => row.matches(".execution:not([data-result-only])"))
    .map(row => [row.dataset.entry, row]));
  // Restore loaded content after polling, but never replace a new result with old live output.
  const expanded = new Map();
  for (const row of previousRows) {
    for (const part of row.querySelectorAll("[data-full]")) {
      if (part.firstElementChild?.hasAttribute("data-expanded")) expanded.set(part.dataset.full, part);
    }
  }
  const previousSaved = previousRows.filter(row => !row.hasAttribute("data-live"));
  // Keep the prefix that rolled out of Tau's bounded snapshot when the pages overlap.
  const overlap = previousSaved.findIndex(row => {
    const incoming = latest.get(row.dataset.entry);
    return incoming && (!incoming.hasAttribute("data-result-only") || row.hasAttribute("data-result-only"));
  });
  if (overlap > 0 || (overlap < 0 && previousSaved.some(row => latest.has(row.dataset.entry)))) {
    // A result may still be in the window after its call rolls out.
    document.querySelector("#recent").append(...previousSaved.slice(0, overlap < 0 ? previousSaved.length : overlap));
  }
  previousRows = [];
  const seen = new Map();
  for (const row of viewport.querySelectorAll("[data-entry]")) {
    for (const part of row.querySelectorAll("[data-full]")) {
      const loaded = expanded.get(part.dataset.full);
      if (loaded && loaded !== part) part.replaceWith(loaded.cloneNode(true));
    }
    const key = row.dataset.entry;
    const older = seen.get(key);
    // If the snapshot gives us only a result, retain the call's header and arguments.
    const call = row.hasAttribute("data-result-only") &&
      (older && !older.hasAttribute("data-result-only") ? older : previousCalls.get(key));
    if (call) {
      call.querySelector(".execution-status").replaceWith(row.querySelector(".execution-status"));
      call.querySelector(".execution-output").replaceWith(row.querySelector(".execution-output"));
      call.removeAttribute("data-live");
      if (call === older) row.remove();
      else {
        older?.remove();
        row.replaceWith(call);
      }
      seen.set(key, call);
    } else {
      older?.remove();
      seen.set(key, row);
    }
  }
  return seen;
}

function requestError(event) {
  const source = event.detail.elt;
  if (source && source === feed) {
    reconnecting = true;
    document.querySelector("#feed-notice").textContent = "Connection lost. Reconnecting…";
    delete feed.lastResponse;
    syncComposer();
  } else if (source?.matches(".show-full")) {
    source.closest("[data-full]").querySelector(".full-notice").textContent = "Could not load the full content. Try again.";
  } else if (source && source === queueForm) {
    queueNotice.textContent = "Could not confirm the edit. Check the queue before trying again.";
  } else if (source?.closest(".instance-actions")) {
    document.querySelector("#lifecycle-notice").textContent = "No confirmation. Check the instance list before trying again.";
  } else if (source?.closest(".mutation")) {
    const notice = source.closest(".start-panel") ? "#start-notice" : "#notice";
    document.querySelector(notice).textContent = "No confirmation. Check Tau before trying again; nothing was resent.";
  }
}

for (const name of ["responseError", "sendError", "timeout"]) {
  document.body.addEventListener(`htmx:${name}`, requestError);
}

// Show progress on the instance card itself; the list doesn't refresh under it.
let lifecycleBusy = false;
document.body.addEventListener("htmx:beforeRequest", event => {
  const button = event.detail.elt?.closest?.(".instance-actions form")?.querySelector("[data-busy-label]");
  if (!button || event.defaultPrevented) return;
  lifecycleBusy = true;
  button.dataset.label = button.textContent;
  button.textContent = button.dataset.busyLabel;
  document.querySelector("#lifecycle-notice").textContent = "";
});
document.body.addEventListener("htmx:afterRequest", event => {
  const button = event.detail.elt?.closest?.(".instance-actions form")?.querySelector("[data-busy-label]");
  if (!button) return;
  lifecycleBusy = false;
  if (button.dataset.label) button.textContent = button.dataset.label;
});

document.body.addEventListener("htmx:beforeRequest", event => {
  const source = event.detail.elt;
  if (source && (source === feed || source === instances)) {
    const refreshing = event.detail.requestConfig.triggeringEvent?.type === "instanceListRefresh";
    if (!refreshing && (document.hidden || (source === instances && instances.contains(document.activeElement)))) {
      event.preventDefault();
    }
  }
  if (composer && source === composer) {
    if (!promptBox.value.trim() && !attachments.files.length) {
      document.querySelector("#notice").textContent = "Enter a message or attach a file.";
      event.preventDefault();
      return;
    }
    submittedText = promptBox.value;
    submittedFiles = attachments.files;
    sending = true;
  }
});

document.body.addEventListener("promptAccepted", () => {
  if (promptBox && promptBox.value === submittedText) {
    promptBox.value = "";
    saveDraft();
  }
  if (attachments && attachments.files === submittedFiles) {
    attachments.value = "";
    renderAttachments();
  }
  followLatest = true;
});

document.body.addEventListener("htmx:afterRequest", event => {
  const source = event.detail.elt;
  if (composer && source === composer) {
    sending = false;
    syncComposer();
  }
  if (event.detail.successful && source?.closest(".mutation") && !source.closest(".start-panel")) {
    htmx.trigger(document.body, "conversationRefresh");
  }
});

document.body.addEventListener("htmx:beforeSwap", event => {
  const target = event.detail.target;
  if (target === instances) {
    const response = event.detail.serverResponse.trim();
    const refreshing = event.detail.requestConfig.triggeringEvent?.type === "instanceListRefresh";
    if (lifecycleBusy || (!refreshing && (instances.contains(document.activeElement) || instances.lastResponse === response))) {
      event.detail.shouldSwap = false;
    } else if (event.detail.shouldSwap) {
      instances.lastResponse = response;
    }
  } else if (target === feed) {
    if (!event.detail.shouldSwap) return;
    const incoming = event.detail.serverResponse.trim();
    if (feed.lastResponse === incoming) {
      event.detail.shouldSwap = false;
      return;
    }
    const top = viewport.getBoundingClientRect().top;
    const anchor = Array.from(viewport.querySelectorAll("[data-entry]:not([data-live])"))
      .find(row => row.getBoundingClientRect().bottom > top);
    scrollAnchor = anchor && [anchor.dataset.entry, anchor.getBoundingClientRect().top - top];
    const active = document.activeElement;
    focusedSummary = active.matches(".execution summary") ? active.closest(".execution").dataset.entry : null;
    openExecutions = new Set(Array.from(viewport.querySelectorAll(".execution details[open]"), detail => detail.parentElement.dataset.entry));
    previousRows = Array.from(feed.querySelectorAll("[data-entry]"));
    followLatest ||= !active.closest(".request, .execution") && viewport.scrollHeight - viewport.clientHeight - viewport.scrollTop < 80;
    feed.lastResponse = incoming;
  } else if (target?.matches("[data-full]") && event.detail.shouldSwap) {
    const response = new DOMParser().parseFromString(event.detail.serverResponse, "text/html");
    const problem = response.body.firstElementChild;
    if (problem?.hasAttribute("data-content-error")) {
      event.detail.shouldSwap = false;
      target.querySelector(".full-notice").textContent = problem.textContent;
    }
  } else if (target?.matches(".history-control")) {
    oldHeight = viewport.scrollHeight;
  }
});

document.body.addEventListener("htmx:afterSettle", event => {
  if (event.detail.target === feed) {
    const rows = reconcileRows();
    for (const key of openExecutions) {
      const detail = rows.get(key)?.querySelector("details");
      if (detail) detail.open = true;
    }
    if (focusedSummary) rows.get(focusedSummary)?.querySelector("summary")?.focus({preventScroll: true});
    reconnecting = false;
    document.querySelector("#feed-notice").textContent = "";
    syncQueue();
    syncComposer();
    if (followLatest) bottom();
    else if (scrollAnchor) {
      const [key, offset] = scrollAnchor;
      const row = rows.get(key);
      if (row) viewport.scrollTop += row.getBoundingClientRect().top - viewport.getBoundingClientRect().top - offset;
    }
    followLatest = false;
  } else if (oldHeight !== undefined && event.detail.elt?.closest?.("#archive")) {
    reconcileRows();
    viewport.scrollTop += viewport.scrollHeight - oldHeight;
    oldHeight = undefined;
  }
});

document.addEventListener("visibilitychange", () => {
  if (!document.hidden && feed) htmx.trigger(document.body, "conversationRefresh");
});

// ---------- Session controls ----------

const controls = document.querySelector("#controls");
const controlsBody = document.querySelector("#controls-body");
const palette = document.querySelector("#palette");

// Meter fills are set here because the content policy blocks inline styles.
function applyMeters() {
  for (const bar of document.querySelectorAll("[data-used]")) {
    bar.style.inlineSize = `${Math.min(100, Math.max(0, Number(bar.dataset.used) || 0))}%`;
  }
}

function controlsURL(tab) {
  const session = composer?.elements.session.value ?? "";
  return `${location.pathname}/controls?tab=${tab}&session=${encodeURIComponent(session)}`;
}

function selectTab(tab) {
  for (const t of controls.querySelectorAll("[role=tab]")) t.setAttribute("aria-selected", String(t.dataset.tab === tab));
}

function openControls(tab, request) {
  if (!controls) return;
  selectTab(tab);
  controlsBody.innerHTML = '<p class="hint">Loading…</p>';
  if (request) request();
  else htmx.ajax("GET", controlsURL(tab), { target: controlsBody });
  if (!controls.open) controls.showModal();
}

// Runs a panel action from the command menu and shows its result in the panel.
function controlAction(tab, action, value = "") {
  const session = composer.elements.session.value;
  openControls(tab, () => htmx.ajax("POST", `${location.pathname}/controls`, {
    target: controlsBody, values: { action, value, session, tab },
  }));
}

function applyModelFilter() {
  const list = controlsBody?.querySelector("#model-list");
  if (!list) return;
  const query = controlsBody.querySelector('[data-filter-list="#model-list"]')?.value.trim().toLowerCase() ?? "";
  const scopedOnly = list.hasAttribute("data-scoped-only");
  for (const option of list.querySelectorAll(".option")) {
    const inScope = !scopedOnly || option.hasAttribute("data-scoped") || option.getAttribute("aria-checked") === "true";
    option.hidden = !inScope || !option.textContent.toLowerCase().includes(query);
  }
}

document.addEventListener("click", event => {
  const open = event.target.closest?.("[data-open]");
  if (open) openControls(open.dataset.open);
  if (!controls?.contains(event.target)) return;
  if (event.target === controls || event.target.closest("[data-close]")) controls.close();
  const tab = event.target.closest("[role=tab]");
  if (tab) openControls(tab.dataset.tab);
  const scope = event.target.closest("[data-scope]");
  if (scope) {
    for (const b of controlsBody.querySelectorAll("[data-scope]")) b.setAttribute("aria-pressed", String(b === scope));
    controlsBody.querySelector("#model-list").toggleAttribute("data-scoped-only", scope.dataset.scope === "scoped");
    applyModelFilter();
  }
  const copy = event.target.closest("[data-copy-reply]");
  if (copy) copyLastReply(copy);
});

controls?.addEventListener("input", event => {
  const selector = event.target.dataset.filterList;
  if (!selector) return;
  if (selector === "#model-list") return applyModelFilter();
  const query = event.target.value.trim().toLowerCase();
  for (const row of controlsBody.querySelectorAll(`${selector} > li`)) {
    row.hidden = !row.textContent.toLowerCase().includes(query);
  }
});

async function copyLastReply(button) {
  const label = button.textContent;
  try {
    const response = await fetch(button.dataset.copyReply);
    if (!response.ok) throw new Error(await response.text());
    const text = await response.text();
    if (!text) throw new Error("There is no reply yet.");
    await navigator.clipboard.writeText(text);
    button.textContent = "Copied";
  } catch {
    button.textContent = "Couldn't copy";
  }
  setTimeout(() => { button.textContent = label; }, 2000);
}

document.body.addEventListener("htmx:afterSettle", event => {
  applyMeters();
  if (event.detail.target === controlsBody) applyModelFilter();
});
document.body.addEventListener("htmx:oobAfterSwap", applyMeters);

// ---------- Command menu ----------

// Built-in commands map to the controls panel. Skill, prompt, and plugin
// commands from Tau are typed into the message box so you can add arguments.
const builtinCommands = [
  { name: "model", description: "Switch the active model", run: () => openControls("model") },
  { name: "thinking", description: "Set the thinking level", run: () => openControls("model") },
  { name: "compact", description: "Compact session history", run: () => openControls("session") },
  { name: "retry", description: "Resume an interrupted or failed run", run: () => controlAction("session", "retry") },
  { name: "new", description: "Start a new session", run: () => { if (confirm("Start a new session? This one stays in History.")) controlAction("session", "new"); } },
  { name: "resume", description: "Resume a saved session", run: () => openControls("sessions") },
  { name: "fork", description: "Fork from a message", run: () => openControls("session", () => htmx.ajax("GET", `${location.pathname}/controls/pick?kind=fork&session=${encodeURIComponent(composer.elements.session.value)}`, { target: controlsBody })) },
  { name: "forget", description: "Drop messages from a picked point onward", run: () => openControls("session", () => htmx.ajax("GET", `${location.pathname}/controls/pick?kind=forget&session=${encodeURIComponent(composer.elements.session.value)}`, { target: controlsBody })) },
  { name: "name", description: "Name this session", run: () => openControls("session") },
  { name: "copy", description: "Copy the last reply", run: () => { const b = document.createElement("button"); b.dataset.copyReply = `${location.pathname}/last-reply`; copyLastReply(b).then(() => { document.querySelector("#notice").textContent = b.textContent === "Copied" ? "Copied the last reply." : "Couldn't copy the last reply."; }); } },
  { name: "export", description: "Download the session as HTML", run: () => { location.href = `${location.pathname}/export`; } },
  { name: "reload", description: "Reload plugins and resources", run: () => controlAction("session", "reload") },
  { name: "mcp", description: "Manage MCP servers", run: () => openControls("tools") },
  { name: "plugins", description: "Enable or disable plugins", run: () => openControls("tools") },
  { name: "settings", description: "Open session controls", run: () => openControls("session") },
];
let extraCommands;
let paletteMatches = [];
let paletteActive = 0;

async function loadCommands() {
  if (extraCommands) return;
  extraCommands = [];
  try {
    const response = await fetch(promptBox.dataset.commands);
    if (!response.ok) return;
    const names = new Set(builtinCommands.map(c => c.name));
    extraCommands = (await response.json()).filter(c => !names.has(c.name)).map(c => ({
      name: c.name, description: c.description,
      run: () => { promptBox.value = `/${c.name} `; promptBox.focus(); saveDraft(); },
    }));
  } catch { /* The built-in commands still work. */ }
}

function updatePalette() {
  if (!palette) return;
  const match = /^\/(\S*)$/.exec(promptBox.value);
  if (!match) { palette.hidden = true; return; }
  loadCommands().then(() => { if (!palette.hidden || paletteMatches.length === 0) renderPalette(match[1]); });
  renderPalette(match[1]);
}

function renderPalette(prefix) {
  const query = prefix.toLowerCase();
  paletteMatches = [...builtinCommands, ...(extraCommands ?? [])].filter(c => c.name.toLowerCase().startsWith(query));
  paletteActive = Math.min(paletteActive, Math.max(paletteMatches.length - 1, 0));
  palette.replaceChildren(...paletteMatches.map((command, i) => {
    const item = document.createElement("li");
    item.role = "option";
    item.setAttribute("aria-selected", String(i === paletteActive));
    const name = document.createElement("span");
    name.className = "cmd-name";
    name.textContent = `/${command.name}`;
    const hint = document.createElement("span");
    hint.className = "row-hint";
    hint.textContent = command.description ?? "";
    item.append(name, hint);
    // Keep focus in the message box, but run only on a tap so dragging scrolls the list.
    item.addEventListener("mousedown", event => event.preventDefault());
    item.addEventListener("click", () => runCommand(command));
    return item;
  }));
  palette.hidden = paletteMatches.length === 0;
}

function runCommand(command) {
  promptBox.value = "";
  saveDraft();
  palette.hidden = true;
  command.run();
}

promptBox?.addEventListener("input", () => { paletteActive = 0; updatePalette(); });
// paletteKey handles navigation while the command menu is open.
function paletteKey(event) {
  if (!palette || palette.hidden || event.isComposing) return false;
  const count = paletteMatches.length;
  if (event.key === "ArrowDown" || event.key === "ArrowUp") {
    event.preventDefault();
    paletteActive = (paletteActive + (event.key === "ArrowDown" ? 1 : count - 1)) % count;
    renderPalette(promptBox.value.slice(1));
  } else if ((event.key === "Enter" && !event.ctrlKey && !event.metaKey) || event.key === "Tab") {
    event.preventDefault();
    runCommand(paletteMatches[paletteActive]);
  } else if (event.key === "Escape") {
    palette.hidden = true;
  } else {
    return false;
  }
  return true;
}
if (controls) applyMeters();
syncComposer();
bottom();
