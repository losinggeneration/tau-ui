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
    if (!refreshing && (instances.contains(document.activeElement) || instances.lastResponse === response)) {
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
syncComposer();
bottom();
