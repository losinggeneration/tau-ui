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
const browseProject = document.querySelector("#browse-project");
const projectPicker = document.querySelector("#project-picker");
const projectDirList = document.querySelector("#project-dir-list");
const projectDirNotice = document.querySelector("#project-dir-notice");
let submittedText;
let submittedFiles;
let openExecutions = new Set();
let previousRows = [];
let scrollAnchor;
let focusedSummary;
let reconnecting = false;
let draftKey;
let sending = false;
let following = true;
let lastScrollTop = 0;
let swappingFeed = false;
let oldHeight;
let queueOpening = false;
let queueSaved = false;
let usageWaitTimer;

// ---------- Toasts ----------

const toastList = document.querySelector("#toast-list");
const progress = document.querySelector("#progress");
const progressList = document.querySelector("#progress-list");
const toastLife = 6000;

// Errors stay until dismissed. Anything else clears itself unless it's in use.
function toast(text, failed = false) {
  text = text.trim();
  if (!toastList || !text) return;
  for (const old of [...toastList.children]) if (old.dataset.text === text) old.remove();
  const item = document.createElement("li");
  item.className = `notice toast${failed ? " error" : ""}`;
  item.dataset.text = text;
  const body = document.createElement("span");
  body.className = "toast-text";
  body.textContent = text;
  const close = document.createElement("button");
  close.type = "button";
  close.className = "quiet toast-close";
  close.textContent = "×";
  close.setAttribute("aria-label", `Dismiss: ${text}`);
  close.addEventListener("click", () => {
    item.remove();
    // Not the message box: focusing it would raise the keyboard on phones.
    (toastList.querySelector("button") ?? viewport).focus({preventScroll: true});
  });
  item.append(body, close);
  toastList.append(item);
  if (failed) return;
  const expire = () => {
    if (item.matches(":hover, :focus-within")) setTimeout(expire, toastLife);
    else item.remove();
  };
  setTimeout(expire, toastLife);
}

// Progress toasts can't be dismissed. They last until their work ends.
function setProgress(key, text) {
  if (!progress) return;
  const item = [...progressList.children].find(row => row.dataset.key === key);
  if (!text) item?.remove();
  else if (item) item.textContent = text;
  else {
    const row = document.createElement("li");
    row.className = "notice toast";
    row.dataset.key = key;
    row.textContent = text;
    progressList.append(row);
    // A collapsed list isn't announced, so screen readers get this instead.
    document.querySelector("#progress-live").textContent = text;
  }
  renderProgress();
}

function renderProgress() {
  const count = progressList.children.length;
  progress.hidden = !count;
  document.querySelector("#progress-count").textContent = count;
  document.querySelector("#progress-label").textContent =
    progress.open || !count ? "In progress" : progressList.lastElementChild.textContent;
}

if (progress) {
  let saved = null;
  try { saved = localStorage.getItem("tau-ui:progress-open"); }
  catch { /* The browser can disable storage. */ }
  // Phones start collapsed. This matches the single-pane breakpoint in style.css.
  progress.open = saved ? saved === "true" : !matchMedia("(max-width: 55.99rem), (max-width: 71.99rem) and (orientation: portrait)").matches;
  // Saved on click because setting .open above also fires toggle.
  progress.querySelector("summary").addEventListener("click", () => {
    try { localStorage.setItem("tau-ui:progress-open", String(!progress.open)); }
    catch { /* Keep the setting for this page. */ }
  });
  progress.addEventListener("toggle", renderProgress);
}

// Server responses land in the hidden #notice, which htmx targets.
if (toastList) {
  const sink = document.querySelector("#notice");
  new MutationObserver(() => toast(sink.textContent, !!sink.querySelector(".error")))
    .observe(sink, {childList: true, subtree: true, characterData: true});
}

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

// Tick in the browser: changing the feed HTML every second would force a full swap.
function showUsageWait() {
  const wait = document.querySelector("[data-resets-at]");
  if (!wait) {
    if (usageWaitTimer) usageWaitTimer = clearInterval(usageWaitTimer);
    return;
  }
  usageWaitTimer ??= setInterval(showUsageWait, 1000);
  const left = Math.ceil((Date.parse(wait.dataset.resetsAt) - Date.now()) / 1000);
  if (Number.isNaN(left)) return;
  const hours = Math.floor(left / 3600);
  const minutes = Math.floor(left / 60) % 60;
  const time = `${hours ? `${hours}h ` : ""}${hours || minutes ? `${minutes}m ` : ""}${left % 60}s`;
  // Tau retries shortly after the reset, so hold here until the next snapshot.
  wait.querySelector("span").textContent = left > 0 ? `retrying in ${time}` : "retrying now";
}

function syncQueue() {
  const state = feed.firstElementChild;
  const count = state.dataset.queueCount;
  const items = state.querySelector("#queue-update");
  const paused = state.dataset.queuePaused === "true";
  // A paused, empty queue stays visible so it can still be resumed.
  queuePanel.hidden = !Number(count) && !paused;
  document.querySelector("#queue-paused").hidden = !paused;
  document.querySelector("#queue-count").textContent = count;
  if (items.innerHTML === queueItems.innerHTML) return;
  const focusedID = document.activeElement?.dataset.queueEdit;
  queueItems.replaceChildren(items.content.cloneNode(true));
  if (focusedID !== undefined) queueItems.querySelector(`[data-queue-edit="${CSS.escape(focusedID)}"]`)?.focus({preventScroll: true});
}

// requestError reports failures, so the promise only needs to settle.
function queueRequest(action, values, target) {
  return htmx.ajax("POST", `${location.pathname}/queue/${action}`, {source: queuePanel, target, values}).catch(() => {});
}

queueItems?.addEventListener("click", event => {
  const session = feed.firstElementChild.dataset.session;
  const edit = event.target.closest("[data-queue-edit]");
  if (edit && !queueOpening && !queueEditor.open) {
    queueOpening = true;
    queueRequest("pause", {session, id: edit.dataset.queueEdit}, "#queue-editor-body")
      .finally(() => { queueOpening = false; });
  } else if (event.target.closest("[data-queue-resume]")) {
    queuePanel.querySelector("summary").focus({preventScroll: true});
    queueRequest("resume", {session}, "#notice");
  }
});
document.body.addEventListener("queueEditOpen", () => {
  queueSaved = false;
  queueEditor.showModal();
  queueEditor.querySelector("textarea:not([readonly])")?.focus();
});
queueEditor?.addEventListener("click", event => {
  if (event.target.closest("#queue-cancel")) queueEditor.close();
});
queueEditor?.addEventListener("cancel", event => {
  if (queueEditor.querySelector("form.htmx-request")) event.preventDefault();
});
// Cancel and Escape both land here. Saving resumes on the server instead.
queueEditor?.addEventListener("close", () => {
  const form = queueEditor.querySelector("form");
  if (queueSaved || !form) return;
  const session = form.elements.session.value;
  const queue = new FormData(form).getAll("resume");
  form.remove();
  if (queue.length) queueRequest("resume", {session, queue}, "#notice");
});
document.body.addEventListener("queueEditSaved", () => {
  queueSaved = true;
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
  if (!viewport) return;
  viewport.scrollTop = viewport.scrollHeight;
  lastScrollTop = viewport.scrollTop;
  following = true;
}

const distanceFromBottom = () => viewport.scrollHeight - viewport.clientHeight - viewport.scrollTop;
const pastFollowLimit = () => distanceFromBottom() > viewport.clientHeight * 0.05;

// Only upward movement stops following, so content growing under a pinned view can't.
function trackScroll() {
  if (swappingFeed) return;
  const movedUp = viewport.scrollTop < lastScrollTop;
  lastScrollTop = viewport.scrollTop;
  if (distanceFromBottom() <= 2) following = true;
  else if (movedUp && pastFollowLimit()) following = false;
}

// Expanding a row pushes the bottom away; stay on what the reader just opened.
function stopFollowingIfExpanded() {
  if (!swappingFeed && pastFollowLimit()) following = false;
}

if (viewport) {
  const keepPinned = () => { if (following) bottom(); };
  viewport.addEventListener("scroll", trackScroll, {passive: true});
  viewport.addEventListener("load", keepPinned, true);
  viewport.addEventListener("toggle", stopFollowingIfExpanded, true);
  if (globalThis.ResizeObserver) new ResizeObserver(keepPinned).observe(viewport);
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

// Moves this page to a restarted instance without reloading, so the draft and
// any chosen attachments stay put. The session, and so the draft key, is unchanged.
function followInstance(id) {
  const from = location.pathname;
  const to = `/worker/${encodeURIComponent(id)}`;
  if (from === to || !from.startsWith("/worker/")) return;
  const active = `/instances?active=${from.slice("/worker/".length)}`;
  const routes = ["hx-get", "hx-post", "action", "src", "data-commands", "data-copy-reply", "data-full"];
  history.replaceState(null, "", to);
  for (const element of document.querySelectorAll(routes.map(name => `[${name}]`).join())) {
    for (const name of routes) {
      const value = element.getAttribute(name);
      if (value?.startsWith(`${from}/`)) element.setAttribute(name, to + value.slice(from.length));
      else if (value === active) element.setAttribute(name, `/instances?active=${encodeURIComponent(id)}`);
    }
  }
  htmx.process(document.body);
  htmx.trigger(document.body, "instanceListRefresh");
  htmx.trigger(document.body, "conversationRefresh");
}

function requestError(event) {
  const source = event.detail.elt;
  if (source && source === feed) {
    const replacement = event.detail.xhr?.getResponseHeader?.("X-Tau-Replacement");
    const state = event.detail.xhr?.getResponseHeader?.("X-Tau-Instance-State");
    reconnecting = true;
    setProgress("connection", replacement ? "Instance restarted. Reconnecting…" :
      state === "restarting" ? "Instance restarting…" :
      state === "exited" ? "Worker exited." : "Connection lost. Reconnecting…");
    // The next feed must swap even if unchanged, since that clears this state.
    delete feed.lastResponse;
    document.querySelector(".usage-wait")?.remove();
    showUsageWait();
    syncComposer();
    if (replacement) followInstance(replacement);
  } else if (source?.matches(".show-full")) {
    source.closest("[data-full]").querySelector(".full-notice").textContent = "Could not load the full content. Try again.";
  } else if (source?.id === "queue-edit-form") {
    document.querySelector("#queue-edit-notice").textContent = "Could not confirm the edit. Check the queue before trying again.";
  } else if (source?.closest(".instance-card")) {
    const problem = document.createElement("span");
    problem.className = "error";
    problem.textContent = "No confirmation. Check the instance list before trying again.";
    document.querySelector("#lifecycle-notice").append(problem);
  } else if (browseProject && (source === browseProject || projectPicker.contains(source))) {
    projectDirNotice.textContent = "Could not load folders. Try again or enter the path manually.";
    projectDirNotice.classList.add("error");
  } else if (source?.id === "notice") {
    toast("No confirmation. Check Tau before trying again; nothing was resent.", true);
  } else if (source && controlsBody?.contains(source)) {
    const notice = document.createElement("p");
    notice.className = "notice error";
    notice.textContent = "No confirmation. Check Tau before trying again; nothing was resent.";
    controlsBody.prepend(notice);
    controlsBody.scrollTop = 0;
  } else if (source?.closest(".mutation")) {
    const text = "No confirmation. Check Tau before trying again; nothing was resent.";
    if (source.closest(".start-panel")) document.querySelector("#start-notice").textContent = text;
    else toast(text, true);
  }
}

for (const name of ["responseError", "sendError", "timeout"]) {
  document.body.addEventListener(`htmx:${name}`, requestError);
}

projectPicker?.addEventListener("click", event => {
  const folder = event.target.closest("[data-project-path]");
  if (!folder) return;
  const directory = document.querySelector("#directory");
  directory.value = folder.dataset.projectPath;
  projectPicker.close();
  // The submit button is disabled during the request, so keep focus on the field.
  directory.focus();
  browseProject.form.requestSubmit();
});

document.body.addEventListener("htmx:beforeRequest", event => {
  if (!projectPicker || event.detail.target !== projectDirList || event.defaultPrevented) return;
  if (event.detail.elt === browseProject) {
    projectDirList.replaceChildren();
    browseProject.focus();
    projectPicker.showModal();
  }
  projectDirNotice.classList.remove("error");
  projectDirNotice.textContent = event.detail.elt.matches(".project-dir-search") ? "Searching folders…" : "Loading folders…";
});

document.body.addEventListener("htmx:afterSwap", event => {
  if (event.detail.target !== projectDirList || !projectPicker?.open) return;
  projectDirNotice.textContent = "";
  projectPicker.scrollTop = 0;
  (projectDirList.querySelector("[data-project-search-status]") ??
    projectDirList.querySelector("[data-project-location]"))?.focus({preventScroll: true});
});

// Working and Waiting persist across visits; Done starts closed on each page.
const instanceGroups = { working: true, waiting: true, done: false };
for (const name of ["working", "waiting"]) {
  try { instanceGroups[name] = localStorage.getItem(`tau-ui:${name}-open`) !== "false"; }
  catch { /* The browser can disable storage. */ }
}

function restoreInstanceGroups() {
  for (const group of instances?.querySelectorAll("details[data-instance-group]") ?? []) {
    group.open = instanceGroups[group.dataset.instanceGroup];
  }
}

instances?.addEventListener("toggle", event => {
  const group = event.target;
  const name = group.dataset.instanceGroup;
  if (!Object.hasOwn(instanceGroups, name) || !instances.contains(group)) return;
  instanceGroups[name] = group.open;
  if (name !== "done") {
    try { localStorage.setItem(`tau-ui:${name}-open`, String(group.open)); }
    catch { /* Keep the setting in memory if storage isn't available. */ }
  }
}, true);
restoreInstanceGroups();

function instanceCardFocused() {
  return instances?.contains(document.activeElement) && !!document.activeElement.closest(".instance-card");
}

// Show progress on the instance card itself; the list doesn't refresh under it.
// Tracked by request because htmx reports afterRequest twice for a removed element.
const lifecycleRequests = new Set();
let listRequest = null;
let listStale = false;

// The list's sync drops a refresh while one is loading, and its swap waits for
// card actions, so hold the refresh until both are done.
function refreshStaleList() {
  if (!listStale || listRequest || lifecycleRequests.size) return;
  listStale = false;
  htmx.trigger(document.body, "instanceListRefresh");
}

document.body.addEventListener("htmx:beforeRequest", event => {
  const button = event.detail.elt?.closest?.(".instance-card form")?.querySelector("[data-busy-label]");
  if (!button || event.defaultPrevented) return;
  // Errors from a batch stay up until the next batch starts.
  if (!lifecycleRequests.size) document.querySelector("#lifecycle-notice").textContent = "";
  lifecycleRequests.add(event.detail.xhr);
  button.dataset.label = button.textContent;
  button.textContent = button.dataset.busyLabel;
});
document.body.addEventListener("htmx:afterRequest", event => {
  const button = event.detail.elt?.closest?.(".instance-card form")?.querySelector("[data-busy-label]");
  if (!button || !lifecycleRequests.delete(event.detail.xhr)) return;
  if (button.dataset.label) button.textContent = button.dataset.label;
  listStale = true;
  refreshStaleList();
});

document.body.addEventListener("htmx:beforeRequest", event => {
  const source = event.detail.elt;
  if (source && (source === feed || source === instances)) {
    const refreshing = event.detail.requestConfig.triggeringEvent?.type === "instanceListRefresh";
    if (!refreshing && (document.hidden || (source === instances && (lifecycleRequests.size || instanceCardFocused())))) {
      event.preventDefault();
    }
    if (source === instances && !event.defaultPrevented) listRequest = event.detail.xhr;
  }
  if (source?.matches?.(".compact-form form") && !event.defaultPrevented) {
    source.querySelector("button").textContent = "Compacting…";
    setProgress("compact", "Compacting…");
  }
  if (composer && source === composer) {
    if (!promptBox.value.trim() && !attachments.files.length) {
      toast("Enter a message or attach a file.");
      event.preventDefault();
      return;
    }
    if (!attachments.files.length && runTypedCommand()) {
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
  following = true;
});

document.body.addEventListener("htmx:afterRequest", event => {
  const source = event.detail.elt;
  if (source === instances && event.detail.xhr === listRequest) {
    listRequest = null;
    refreshStaleList();
  }
  if (source?.matches?.(".compact-form form")) {
    source.querySelector("button").textContent = "Compact now";
    setProgress("compact");
  }
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
    if (lifecycleRequests.size || (!refreshing && (instanceCardFocused() || instances.lastResponse === response))) {
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
    trackScroll(); // A scroll whose event hasn't fired yet still counts.
    swappingFeed = true;
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

document.body.addEventListener("htmx:afterSwap", event => {
  if (instances && event.detail.target === instances) restoreInstanceGroups();
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
    setProgress("connection");
    syncQueue();
    syncComposer();
    showUsageWait();
    if (following) bottom();
    else if (scrollAnchor) {
      const [key, offset] = scrollAnchor;
      const row = rows.get(key);
      if (row) viewport.scrollTop += row.getBoundingClientRect().top - viewport.getBoundingClientRect().top - offset;
    }
    lastScrollTop = viewport.scrollTop;
    swappingFeed = false;
  } else if (event.detail.target?.matches?.("[data-full]")) {
    stopFollowingIfExpanded();
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

// Runs a command without opening the panel and reports in a toast.
// A failed command puts its typed arguments back in the message box.
function directAction(action, value = "", busy = "") {
  const notice = document.querySelector("#notice");
  notice.textContent = "";
  if (busy) setProgress(action, busy);
  const restore = () => {
    if (busy) setProgress(action);
    if (value && !promptBox.value && !notice.querySelector(".success")) {
      promptBox.value = `/${action} ${value}`;
      saveDraft();
    }
  };
  htmx.ajax("POST", `${location.pathname}/controls`, {
    source: "#notice", target: "#notice",
    values: { action, value, session: composer.elements.session.value, inline: "1" },
  }).then(restore, restore);
}

function applyModelFilter() {
  const list = controlsBody?.querySelector("#model-list");
  if (!list) return;
  const query = controlsBody.querySelector('[data-filter-list="#model-list"]')?.value.trim().toLowerCase() ?? "";
  const scopedOnly = !!controlsBody.querySelector('[data-scope="scoped"][aria-pressed="true"]');
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
    applyModelFilter();
  }
  const copy = event.target.closest("[data-copy-reply]");
  if (copy) copyLastReply(copy);
});

// The viewer fits large images to the screen. Clicking one toggles actual size.
function openImage(source) {
  const viewer = document.querySelector("#image-viewer");
  const image = viewer.querySelector("img");
  image.classList.remove("zoomed", "can-zoom");
  image.removeAttribute("src");
  image.onload = () => image.classList.toggle("can-zoom",
    image.naturalWidth > viewer.clientWidth || image.naturalHeight > viewer.clientHeight);
  image.alt = source.alt;
  image.src = source.currentSrc || source.src;
  viewer.showModal();
}

function toggleImageZoom(event, viewer, image) {
  const before = image.getBoundingClientRect();
  const x = (event.clientX - before.left) / before.width;
  const y = (event.clientY - before.top) / before.height;
  image.classList.toggle("zoomed");
  // Keep the clicked point under the pointer.
  const after = image.getBoundingClientRect();
  viewer.scrollTo(viewer.scrollLeft + after.left + x * after.width - event.clientX,
    viewer.scrollTop + after.top + y * after.height - event.clientY);
}

document.addEventListener("click", event => {
  const source = event.target.closest?.(".message-image");
  if (source) return openImage(source);
  const viewer = event.target.closest?.("#image-viewer");
  if (!viewer) return;
  // A press on the viewer's scrollbars isn't a press on the background.
  if (event.clientX >= viewer.clientWidth || event.clientY >= viewer.clientHeight) return;
  if (event.target.tagName !== "IMG") return viewer.close();
  if (event.target.matches(".zoomed, .can-zoom")) toggleImageZoom(event, viewer, event.target);
});

document.addEventListener("keydown", event => {
  if ((event.key !== "Enter" && event.key !== " ") || !event.target.matches?.(".message-image")) return;
  event.preventDefault();
  openImage(event.target);
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
  if (event.detail.target === controlsBody) {
    applyModelFilter();
    // Actions sit below the fold, and their result shows at the top.
    if (controlsBody.querySelector(":scope > .notice")) controlsBody.scrollTop = 0;
  }
});
document.body.addEventListener("htmx:oobAfterSwap", applyMeters);

// ---------- Command menu ----------

// Built-in commands either run directly or open their part of the controls
// panel. Skill, prompt, and plugin commands from Tau are typed into the
// message box so you can add arguments. `args` commands accept typed text;
// picking /compact from the menu leaves room to add instructions before Send.
const builtinCommands = [
  { name: "model", description: "Switch the active model", run: () => openControls("model") },
  { name: "thinking", description: "Set the thinking level", run: () => openControls("model") },
  { name: "compact", description: "Compact session history", args: true, pick: () => { promptBox.value = "/compact "; promptBox.focus(); saveDraft(); }, run: (text = "") => directAction("compact", text, "Compacting…") },
  { name: "retry", description: "Resume an interrupted or failed run", run: () => directAction("retry") },
  { name: "new", description: "Start a new session", run: () => directAction("new") },
  { name: "resume", description: "Resume a saved session", run: () => openControls("sessions") },
  { name: "fork", description: "Fork from a message", run: () => openControls("session", () => htmx.ajax("GET", `${location.pathname}/controls/pick?kind=fork&session=${encodeURIComponent(composer.elements.session.value)}`, { target: controlsBody })) },
  { name: "forget", description: "Drop messages from a picked point onward", run: () => openControls("session", () => htmx.ajax("GET", `${location.pathname}/controls/pick?kind=forget&session=${encodeURIComponent(composer.elements.session.value)}`, { target: controlsBody })) },
  { name: "name", description: "Name this session", args: true, run: text => text ? directAction("name", text) : openControls("session") },
  { name: "copy", description: "Copy the last reply", run: () => { const b = document.createElement("button"); b.dataset.copyReply = `${location.pathname}/last-reply`; copyLastReply(b).then(() => { const copied = b.textContent === "Copied"; toast(copied ? "Copied the last reply." : "Couldn't copy the last reply.", !copied); }); } },
  { name: "export", description: "Download the session as HTML", run: () => { location.href = `${location.pathname}/export`; } },
  { name: "reload", description: "Reload plugins and resources", run: () => directAction("reload") },
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
  (command.pick ?? command.run)();
}

// Send runs a typed built-in command instead of posting it as a message.
// Text after a command that takes none is left to go to Tau as typed.
function runTypedCommand() {
  const match = /^\/(\S+)(?:\s+([\s\S]*))?$/.exec(promptBox.value.trim());
  const command = match && builtinCommands.find(c => c.name === match[1]);
  if (!command || (match[2] && !command.args)) return false;
  promptBox.value = "";
  saveDraft();
  if (palette) palette.hidden = true;
  command.run(match[2]);
  return true;
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
showUsageWait();
bottom();
