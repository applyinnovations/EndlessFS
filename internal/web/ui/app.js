"use strict";

(() => {
  const state = {
    config: null,
    user: null,
    currentDirectory: "/",
    entries: [],
    nextCursor: "",
    directoryLoading: false,
    directoryRequest: 0,
    selected: new Map(),
    transfers: [],
    transferByID: new Map(),
    transferGroups: new Map(),
    directoryPromises: new Map(),
    activeTransfers: 0,
    transferFilter: "current",
    transferSearch: "",
    transferRenderFrame: 0,
    transferStructureFrame: 0,
    transferProjection: [],
    transferQueueCursor: 0,
    transferSummary: null,
    transferVirtualStart: 0,
    transferVirtualFrame: 0,
    expandedTransferGroups: new Set(),
    transferRetryTimers: new Map(),
    transferFailureTimes: [],
    transferCircuitOpenUntil: 0,
    transferLedger: null,
    transferLedgerOwner: "",
    transferLedgerWarningShown: false,
    transferPersistQueue: new Map(),
    transferPersistTimer: 0,
    transferReconnectGroup: null,
    transferSheetOpener: null,
    transferFixtureTimer: 0,
    themes: [],
    passkeys: [],
    shares: [],
    publicToken: "",
    publicPath: "/",
    publicCursor: "",
    publicLoading: false,
    trashCursor: "",
    trashLoading: false,
    trashRequest: 0,
    browserAccess: "owner",
    inviteToken: "",
    recoveryToken: "",
    dialogOpener: null,
    viewMode: "list",
    filteredEntries: [],
    themeAssets: {},
    previewStates: new Map(),
    previewQueue: [],
    previewQueued: new Set(),
    previewActive: 0,
    previewControllers: new Map(),
    previewObjectURLs: new Map(),
    previewRetryAttempts: new Map(),
    previewRetryTimers: new Map(),
    previewGenerationRequests: new Map(),
    gridObserver: null,
    gridRenderFrame: 0,
    listRenderFrame: 0,
    passkeyRenderFrame: 0,
    shareRenderFrame: 0,
    viewerEntries: [],
    viewerIndex: -1,
    viewerObjectURL: "",
    viewerPreviewCache: new Map(),
    viewerPreviewCacheBytes: 0,
    viewerEntry: null,
    viewerController: null,
    viewerOpener: null,
    safeTheme: new URLSearchParams(location.search).get("safe-theme") === "1",
  };

  const gridOverscanRows = 3;
  const listOverscanRows = 8;
  const listRowHeight = 36;
  const settingsOverscanRows = 6;
  const settingsRowHeight = 36;
  const previewRetryDelays = [250, 500, 1000, 2000, 4000, 5000];
  const maximumPreviewPolls = 14;
  const maximumViewerPreviewCacheEntries = 8;
  const maximumViewerPreviewCacheBytes = 64 << 20;
  const transferLedgerDatabaseName = "endlessfs-transfer-ledger-v1";
  const transferLedgerVersion = 1;
  const transferVirtualWindowSize = 72;
  const transferVirtualWindowStep = 32;
  const transferVirtualRowHeight = 72;
  const transferDiscoveryBatchSize = 64;
  const transferRetryBaseDelay = 1000;
  const transferRetryMaximumDelay = 60000;
  const transferRetryLimit = 7;
  const transferProgressSampleWeight = 0.35;
  let toastTimer = 0;

  class APIError extends Error {
    constructor(response, problem) {
      super(problem && problem.title ? problem.title : `Request failed (${response.status})`);
      this.name = "APIError";
      this.status = response.status;
      this.code = problem && problem.code ? problem.code : "request_failed";
      this.requestID = problem && problem.requestID ? problem.requestID : "";
    }
  }

  const byID = (id) => document.getElementById(id);
  const text = (tag, value, className) => {
    const node = document.createElement(tag);
    node.textContent = String(value);
    if (className) node.className = className;
    return node;
  };
  const button = (label, action, className) => {
    const node = text("button", label, className);
    node.type = "button";
    node.addEventListener("click", action);
    return node;
  };
  const iconPaths = Object.freeze({
    "arrow-left": ["M5 12l14 0", "M5 12l6 6", "M5 12l6 -6"],
    "arrow-right": ["M5 12l14 0", "M13 18l6 -6", "M13 6l6 6"],
    check: ["M5 12l5 5l10 -10"],
    "chevron-down": ["M6 9l6 6l6 -6"],
    "chevron-up": ["M6 15l6 -6l6 6"],
    copy: ["M7 9.667a2.667 2.667 0 0 1 2.667 -2.667h8.666a2.667 2.667 0 0 1 2.667 2.667v8.666a2.667 2.667 0 0 1 -2.667 2.667h-8.666a2.667 2.667 0 0 1 -2.667 -2.667l0 -8.666", "M4.012 16.737a2.005 2.005 0 0 1 -1.012 -1.737v-10c0 -1.1 .9 -2 2 -2h10c.75 0 1.158 .385 1.5 1"],
    "device-floppy": ["M6 4h9l5 5v9a2 2 0 0 1 -2 2h-12a2 2 0 0 1 -2 -2v-12a2 2 0 0 1 2 -2", "M8 4v6h8v-4", "M8 20v-6h8v6"],
    download: ["M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2 -2v-2", "M7 11l5 5l5 -5", "M12 4l0 12"],
    eye: ["M10 12a2 2 0 1 0 4 0a2 2 0 0 0 -4 0", "M21 12c-2.4 4 -5.4 6 -9 6s-6.6 -2 -9 -6c2.4 -4 5.4 -6 9 -6s6.6 2 9 6"],
    file: ["M14 3v4a1 1 0 0 0 1 1h4", "M17 21h-10a2 2 0 0 1 -2 -2v-14a2 2 0 0 1 2 -2h7l5 5v11a2 2 0 0 1 -2 2"],
    "folder-plus": ["M12 19h-7a2 2 0 0 1 -2 -2v-11a2 2 0 0 1 2 -2h4l3 3h7a2 2 0 0 1 2 2v3.5", "M16 19h6", "M19 16v6"],
    "folder-symlink": ["M3 21v-4a3 3 0 0 1 3 -3h5", "M8 17l3 -3l-3 -3", "M3 11v-5a2 2 0 0 1 2 -2h4l3 3h7a2 2 0 0 1 2 2v8a2 2 0 0 1 -2 2h-8"],
    "folder-up": ["M12 19h-7a2 2 0 0 1 -2 -2v-11a2 2 0 0 1 2 -2h4l3 3h7a2 2 0 0 1 2 2v3.5", "M19 22v-6", "M22 19l-3 -3l-3 3"],
    key: ["M16.555 3.843l3.602 3.602a2.877 2.877 0 0 1 0 4.069l-2.643 2.643a2.877 2.877 0 0 1 -4.069 0l-.301 -.301l-6.558 6.558a2 2 0 0 1 -1.239 .578l-.175 .008h-1.172a1 1 0 0 1 -.993 -.883l-.007 -.117v-1.172a2 2 0 0 1 .467 -1.284l.119 -.13l.414 -.414h2v-2h2v-2l2.144 -2.144l-.301 -.301a2.877 2.877 0 0 1 0 -4.069l2.643 -2.643a2.877 2.877 0 0 1 4.069 0", "M15 9h.01"],
    "key-off": ["M10.144 10.856l-.301 -.301a2.877 2.877 0 0 1 -.251 -3.76m1.908 -1.908l.986 -1.044a2.877 2.877 0 0 1 4.069 0l3.602 3.602a2.877 2.877 0 0 1 0 4.069l-1.043 .986m-2.914 2.914a2.877 2.877 0 0 1 -2.755 -1.257l-.301 -.301l-6.558 6.558a2 2 0 0 1 -1.239 .578l-.175 .008h-1.172a1 1 0 0 1 -1 -1v-1.172a2 2 0 0 1 .586 -1.414l.414 -.414h2v-2h2v-2l2.144 -2.144", "M3 3l18 18"],
    "key-plus": ["M16.555 3.843l3.602 3.602a2.877 2.877 0 0 1 0 4.069l-2.643 2.643a2.877 2.877 0 0 1 -4.069 0l-.301 -.301l-6.558 6.558a2 2 0 0 1 -1.239 .578l-.175 .008h-1.172a1 1 0 0 1 -.993 -.883l-.007 -.117v-1.172a2 2 0 0 1 .467 -1.284l.119 -.13l.414 -.414h2v-2h2v-2l2.144 -2.144l-.301 -.301a2.877 2.877 0 0 1 0 -4.069l2.643 -2.643a2.877 2.877 0 0 1 4.069 0", "M15 9h.01", "M18 16v6", "M15 19h6"],
    "layout-grid": ["M4 5a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v4a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -4", "M14 5a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v4a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -4", "M4 15a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v4a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -4", "M14 15a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v4a1 1 0 0 1 -1 1h-4a1 1 0 0 1 -1 -1l0 -4"],
    list: ["M9 6l11 0", "M9 12l11 0", "M9 18l11 0", "M5 6l0 .01", "M5 12l0 .01", "M5 18l0 .01"],
    "link-off": ["M10 13a5 5 0 0 0 7.54 .54l1 -1a5 5 0 0 0 -7.07 -7.07l-.57 .57", "M14 11a5 5 0 0 0 -7.54 -.54l-1 1a5 5 0 0 0 7.07 7.07l.57 -.57", "M3 3l18 18"],
    logout: ["M14 8v-2a2 2 0 0 0 -2 -2h-7a2 2 0 0 0 -2 2v12a2 2 0 0 0 2 2h7a2 2 0 0 0 2 -2v-2", "M9 12h12l-3 -3", "M18 15l3 -3"],
    refresh: ["M20 11a8.1 8.1 0 0 0 -15.5 -2m-.5 -4v4h4", "M4 13a8.1 8.1 0 0 0 15.5 2m.5 4v-4h-4"],
    restore: ["M3.06 13a9 9 0 1 0 .49 -4.087", "M3 4.001v5h5", "M11 12a1 1 0 1 0 2 0a1 1 0 1 0 -2 0"],
    "share-3": ["M13 4v4c-6.575 1.028 -9.02 6.788 -10 12c-.037 .206 5.384 -5.962 10 -6v4l8 -7l-8 -7"],
    "shield-minus": ["M12.46 20.871c-.153 .046 -.306 .089 -.46 .129a12 12 0 0 1 -8.5 -15a12 12 0 0 0 8.5 -3a12 12 0 0 0 8.5 3a12 12 0 0 1 -.916 9.015", "M16 19h6"],
    "shield-plus": ["M12.462 20.87c-.153 .047 -.307 .09 -.462 .13a12 12 0 0 1 -8.5 -15a12 12 0 0 0 8.5 -3a12 12 0 0 0 8.5 3a12 12 0 0 1 .11 6.37", "M16 19h6", "M19 16v6"],
    trash: ["M4 7l16 0", "M10 11l0 6", "M14 11l0 6", "M5 7l1 12a2 2 0 0 0 2 2h8a2 2 0 0 0 2 -2l1 -12", "M9 7v-3a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v3"],
    "trash-x": ["M4 7h16", "M5 7l1 12a2 2 0 0 0 2 2h8a2 2 0 0 0 2 -2l1 -12", "M9 7v-3a1 1 0 0 1 1 -1h4a1 1 0 0 1 1 1v3", "M10 12l4 4m0 -4l-4 4"],
    transfer: ["M20 10h-16l5 -5", "M4 14h16l-5 5"],
    upload: ["M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2 -2v-2", "M7 9l5 -5l5 5", "M12 4l0 12"],
    "user-check": ["M8 7a4 4 0 1 0 8 0a4 4 0 0 0 -8 0", "M6 21v-2a4 4 0 0 1 4 -4h4", "M15 19l2 2l4 -4"],
    "user-off": ["M8.18 8.189a4.01 4.01 0 0 0 2.616 2.627m3.507 -.545a4 4 0 1 0 -5.59 -5.552", "M6 21v-2a4 4 0 0 1 4 -4h4c.412 0 .81 .062 1.183 .178m2.633 2.618c.12 .38 .184 .785 .184 1.204v2", "M3 3l18 18"],
    "user-plus": ["M8 7a4 4 0 1 0 8 0a4 4 0 0 0 -8 0", "M6 21v-2a4 4 0 0 1 4 -4h4", "M16 19h6", "M19 16v6"],
    x: ["M18 6l-12 12", "M6 6l12 12"],
  });
  const applicationIcon = (name) => {
    const namespace = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(namespace, "svg");
    svg.setAttribute("class", "app-icon"); svg.setAttribute("viewBox", "0 0 24 24"); svg.setAttribute("aria-hidden", "true"); svg.setAttribute("focusable", "false");
    for (const pathData of iconPaths[name] || iconPaths.x) {
      const path = document.createElementNS(namespace, "path"); path.setAttribute("d", pathData); svg.append(path);
    }
    return svg;
  };
  const setIconControl = (node, name, label = node.getAttribute("aria-label")) => {
    node.replaceChildren(applicationIcon(name)); node.dataset.icon = name;
    if (label) { node.setAttribute("aria-label", label); node.dataset.tooltip = label; }
    if (label && node === actionTooltipTarget) activeActionTooltip().textContent = label;
    node.removeAttribute("title");
    return node;
  };
  const iconButton = (name, label, action, className = "", tooltip = label) => {
    const node = button("", action, `${className}${className ? " " : ""}icon-button`);
    setIconControl(node, name, label);
    node.dataset.tooltip = tooltip;
    return node;
  };
  function wireIconControls() {
    document.querySelectorAll("[data-icon]").forEach((control) => setIconControl(control, control.dataset.icon));
  }
  let actionTooltipTarget = null;
  function hideActionTooltip(target = null) {
    if (target && target !== actionTooltipTarget) return;
    if (actionTooltipTarget) actionTooltipTarget.classList.remove("tooltip-anchor");
    actionTooltipTarget = null;
    const tooltip = byID("action-tooltip");
    tooltip.hidden = true;
    tooltip.textContent = "";
    if (tooltip.parentElement !== byID("app")) byID("urgent-status").after(tooltip);
  }
  function activeActionTooltip() {
    const tooltip = byID("action-tooltip");
    const openDialogs = [...document.querySelectorAll("dialog[open]")];
    const host = openDialogs[openDialogs.length - 1] || byID("app");
    if (tooltip.parentElement !== host) host.append(tooltip);
    return tooltip;
  }
  function showActionTooltip(target) {
    const label = target.dataset.tooltip;
    if (!label || target.disabled) return;
    const tooltip = activeActionTooltip();
    actionTooltipTarget = target;
    target.classList.add("tooltip-anchor");
    tooltip.textContent = label;
    tooltip.hidden = false;
  }
  const tooltipTarget = (event) => event.target instanceof Element ? event.target.closest("[data-tooltip]") : null;
  function wireActionTooltips() {
    document.addEventListener("pointerover", (event) => {
      const target = tooltipTarget(event);
      if (target && !target.contains(event.relatedTarget)) showActionTooltip(target);
    });
    document.addEventListener("pointerout", (event) => {
      const target = tooltipTarget(event);
      if (target && !target.contains(event.relatedTarget)) hideActionTooltip(target);
    });
    document.addEventListener("focusin", (event) => {
      const target = tooltipTarget(event);
      if (target) showActionTooltip(target);
    });
    document.addEventListener("focusout", (event) => hideActionTooltip(tooltipTarget(event)));
    document.addEventListener("pointerdown", () => hideActionTooltip());
    window.addEventListener("scroll", () => hideActionTooltip(), true);
    window.addEventListener("resize", () => hideActionTooltip());
  }
  const cookie = (name) => {
    const prefix = `${name}=`;
    const item = document.cookie.split("; ").find((part) => part.startsWith(prefix));
    return item ? decodeURIComponent(item.slice(prefix.length)) : "";
  };
  const csrf = () => cookie("__Host-endlessfs_csrf") || cookie("endlessfs_csrf_dev");
  const idempotencyKey = () => crypto.randomUUID();
  const isMutation = (method) => !["GET", "HEAD"].includes(method);

  function wait(delay, signal) {
    return new Promise((resolve, reject) => {
      if (signal && signal.aborted) { reject(new DOMException("Aborted", "AbortError")); return; }
      const onAbort = () => { window.clearTimeout(timer); reject(new DOMException("Aborted", "AbortError")); };
      const timer = window.setTimeout(() => {
        if (signal) signal.removeEventListener("abort", onAbort);
        resolve();
      }, delay);
      if (signal) signal.addEventListener("abort", onAbort, { once: true });
    });
  }

  async function api(path, options = {}) {
    const method = options.method || "GET";
    const headers = new Headers(options.headers || {});
    headers.set("Accept", "application/json");
    if (options.body !== undefined) headers.set("Content-Type", "application/json");
    if (isMutation(method) && csrf()) headers.set("X-EndlessFS-CSRF", csrf());
    const response = await fetch(path, {
      method,
      headers,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      credentials: "same-origin",
      signal: options.signal,
    });
    if (!response.ok) {
      let problem = null;
      try { problem = await response.json(); } catch { problem = null; }
      throw new APIError(response, problem);
    }
    if (response.status === 204) return null;
    return response.json();
  }

  function announce(message, urgent = false) {
    const region = byID(urgent ? "urgent-status" : "live-status");
    region.textContent = "";
    window.setTimeout(() => { region.textContent = message; }, 20);
  }

  function showState(targetID, message, kind = "") {
    const target = byID(targetID);
    target.textContent = message;
    target.className = `state-panel${kind ? ` ${kind}` : ""}`;
    target.hidden = !message;
  }

  function renderTableLoadingRows(targetID, kind, columns, count = 5) {
    const target = byID(targetID);
    const fragment = document.createDocumentFragment();
    for (let rowIndex = 0; rowIndex < count; rowIndex += 1) {
      const row = document.createElement("tr");
      row.className = `loading-item-row loading-${kind}-row`;
      row.setAttribute("aria-hidden", "true");
      for (let columnIndex = 0; columnIndex < columns; columnIndex += 1) {
        const cell = document.createElement("td");
        if (kind === "file" && columnIndex === 0) cell.className = "owner-only";
        if (columnIndex === columns - 1) cell.className = "table-icon-action-cell";
        cell.append(document.createElement("span"));
        row.append(cell);
      }
      fragment.append(row);
    }
    target.replaceChildren(fragment);
    target.setAttribute("aria-busy", "true");
  }

  function renderRecordLoadingItems(targetID, count = 1) {
    const target = byID(targetID);
    const fragment = document.createDocumentFragment();
    for (let index = 0; index < count; index += 1) {
      const item = document.createElement("li");
      item.className = "loading-record-row";
      item.setAttribute("aria-hidden", "true");
      const main = document.createElement("div");
      main.className = "record-main";
      const copy = document.createElement("div");
      copy.className = "loading-record-copy";
      copy.append(text("span", "", "loading-record-line"), text("span", "", "loading-record-line"));
      main.append(copy, text("span", "", "loading-record-action"));
      item.append(main);
      fragment.append(item);
    }
    target.replaceChildren(fragment);
    target.setAttribute("aria-busy", "true");
  }

  function finishListLoading(targetID) {
    byID(targetID).removeAttribute("aria-busy");
  }

  function syncFilePresentation() {
    const gridEnabled = state.viewMode === "grid";
    byID("drop-target").dataset.presentation = gridEnabled ? "grid" : "list";
    byID("list-presentation").hidden = gridEnabled;
    byID("media-grid").hidden = !gridEnabled;
    return gridEnabled;
  }

  function renderFileLoadingItems() {
    const gridEnabled = syncFilePresentation();
    if (!gridEnabled) {
      renderTableLoadingRows("file-rows", "file", 6, 6);
      return;
    }
    const layer = document.createElement("div");
    layer.className = "loading-media-layer";
    layer.setAttribute("aria-hidden", "true");
    for (let index = 0; index < 12; index += 1) layer.append(text("span", "", "loading-media-item"));
    const content = byID("media-grid-content");
    content.replaceChildren(layer);
    content.setAttribute("aria-busy", "true");
  }

  function clearToast() {
    window.clearTimeout(toastTimer);
    const region = byID("toast-region");
    region.replaceChildren();
    if (region.parentElement !== byID("app")) byID("urgent-status").after(region);
  }

  function activeToastRegion() {
    const region = byID("toast-region");
    const openDialogs = [...document.querySelectorAll("dialog[open]")];
    const host = openDialogs[openDialogs.length - 1] || byID("app");
    if (region.parentElement !== host) host.append(region);
    return region;
  }

  function showTopLayerDialog(dialog) {
    dialog.showModal();
    if (byID("toast-region").childElementCount) activeToastRegion();
  }

  function closeTopLayerDialog(dialog) {
    dialog.close();
    if (byID("toast-region").childElementCount) activeToastRegion();
  }

  function showToast(message, level, duration = 8000) {
    clearToast();
    const toast = document.createElement("div");
    toast.className = `toast ${level}`;
    toast.append(text("span", message), iconButton("x", "Dismiss notification", clearToast, "", "Dismiss"));
    activeToastRegion().append(toast);
    announce(message, level === "error");
    toastTimer = window.setTimeout(clearToast, duration);
  }

  function showActionErrorToast(action, error, fallback) {
    const message = `${action} failed. ${friendlyError(error, fallback)}`;
    showToast(message, "error", 12000);
  }

  function showTrashUndo(items) {
    const recoverable = items.filter((item) => item && item.trashID);
    if (!recoverable.length) return;
    clearToast();
    const toast = document.createElement("div");
    toast.className = "toast success";
    toast.append(text("span", `${recoverable.length} item${recoverable.length === 1 ? "" : "s"} moved to Trash`));
    const undo = button("Undo", async () => {
      undo.disabled = true;
      try {
        for (const item of recoverable) {
          const operation = await api(`/api/v1/trash/${encodeURIComponent(item.trashID)}/restore`, {
            method: "POST",
            headers: { "Idempotency-Key": idempotencyKey() },
            body: { conflict: "rename" },
          });
          await watchOperation(operation.operationID || operation.id);
        }
        clearToast();
        announce(`${recoverable.length} item${recoverable.length === 1 ? "" : "s"} restored.`);
        await loadDirectory(state.currentDirectory);
      } catch (error) {
        clearToast();
        announce(friendlyError(error, "Undo could not be completed."), true);
      }
    }, "quiet");
    toast.append(undo);
    activeToastRegion().append(toast);
    toastTimer = window.setTimeout(clearToast, 10000);
  }

  function friendlyError(error, fallback = "That action could not be completed.") {
    if (!navigator.onLine) return "You appear to be offline. Reconnect and try again.";
    if (error instanceof APIError) {
      const suffix = error.requestID ? ` Reference: ${error.requestID}.` : "";
      if (error.status === 401) return `Your session is no longer active. Sign in again.${suffix}`;
      if (error.status === 403) return `You do not have permission to do that.${suffix}`;
      if (error.status === 404) return `The item is no longer available.${suffix}`;
      if (error.status === 409 || error.status === 412) return `The item changed or a name is already in use.${suffix}`;
      if (error.status === 410) return `This one-time link has expired or was already used.${suffix}`;
      if (error.status === 429) return `Too many attempts. Wait briefly and try again.${suffix}`;
      if (error.status >= 500) return `EndlessFS is temporarily unavailable.${suffix}`;
      return `${error.message}.${suffix}`;
    }
    return fallback;
  }

  function showOnly(id) {
    for (const view of ["loading-view", "auth-view", "registration-view", "public-view", "authenticated-view"]) {
      byID(view).hidden = view !== id;
    }
    byID("app").dataset.state = id.replace("-view", "");
  }

  function syncFileBrowserAccess(access) {
    const surface = byID("file-browser-surface");
    const host = byID(`${access}-browser-host`);
    if (surface.parentElement !== host) host.append(surface);
    state.browserAccess = access;
    surface.dataset.access = access;
    byID("breadcrumbs").setAttribute("aria-label", access === "public" ? "Shared folder path" : "Current folder");
    const collectionLabel = access === "trash" ? "Items in trash" : access === "public" ? "Files in shared folder" : "Files in the current folder";
    byID("file-table-caption").textContent = collectionLabel;
    byID("media-grid").setAttribute("aria-label", collectionLabel);
    if (access !== "trash") state.trashRequest += 1;
    if (access !== "owner") {
      state.directoryRequest += 1;
      state.selected.clear();
      updateSelection();
      byID("drop-target").removeAttribute("aria-describedby");
    } else {
      byID("drop-target").setAttribute("aria-describedby", "drop-help");
    }
  }

  function consumePathTokens() {
    const parts = location.pathname.split("/").filter(Boolean);
    if (parts[0] === "register" && parts[1] === "invite" && parts[2]) {
      state.inviteToken = decodeURIComponent(parts[2]);
      history.replaceState({}, "", "/register");
    } else if (parts[0] === "recover" && parts[1]) {
      state.recoveryToken = decodeURIComponent(parts[1]);
      history.replaceState({}, "", "/recover");
    } else if (parts[0] === "s" && parts[1]) {
      state.publicToken = decodeURIComponent(parts[1]);
    }
  }

  function decodeBase64URL(value) {
    const padding = "=".repeat((4 - value.length % 4) % 4);
    const raw = atob(value.replace(/-/g, "+").replace(/_/g, "/") + padding);
    return Uint8Array.from(raw, (character) => character.charCodeAt(0));
  }

  function encodeBase64URL(value) {
    const bytes = new Uint8Array(value || new ArrayBuffer(0));
    let raw = "";
    for (const byte of bytes) raw += String.fromCharCode(byte);
    return btoa(raw).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
  }

  function creationOptions(options) {
    const publicKey = { ...options };
    publicKey.challenge = decodeBase64URL(publicKey.challenge);
    publicKey.user = { ...publicKey.user, id: decodeBase64URL(publicKey.user.id) };
    publicKey.excludeCredentials = (publicKey.excludeCredentials || []).map((item) => ({ ...item, id: decodeBase64URL(item.id) }));
    return publicKey;
  }

  function requestOptions(options) {
    const publicKey = { ...options };
    publicKey.challenge = decodeBase64URL(publicKey.challenge);
    publicKey.allowCredentials = (publicKey.allowCredentials || []).map((item) => ({ ...item, id: decodeBase64URL(item.id) }));
    return publicKey;
  }

  function registrationCredential(credential) {
    return {
      id: credential.id,
      rawId: encodeBase64URL(credential.rawId),
      type: credential.type,
      authenticatorAttachment: credential.authenticatorAttachment,
      response: {
        clientDataJSON: encodeBase64URL(credential.response.clientDataJSON),
        attestationObject: encodeBase64URL(credential.response.attestationObject),
        transports: typeof credential.response.getTransports === "function" ? credential.response.getTransports() : [],
      },
      clientExtensionResults: credential.getClientExtensionResults(),
    };
  }

  function authenticationCredential(credential) {
    return {
      id: credential.id,
      rawId: encodeBase64URL(credential.rawId),
      type: credential.type,
      authenticatorAttachment: credential.authenticatorAttachment,
      response: {
        clientDataJSON: encodeBase64URL(credential.response.clientDataJSON),
        authenticatorData: encodeBase64URL(credential.response.authenticatorData),
        signature: encodeBase64URL(credential.response.signature),
        userHandle: credential.response.userHandle ? encodeBase64URL(credential.response.userHandle) : null,
      },
      clientExtensionResults: credential.getClientExtensionResults(),
    };
  }

  async function register(flow, displayName, bearer = "", label = "") {
    let startPath = "/api/v1/registration/options";
    let finishPath = "/api/v1/registration/verify";
    let request = { displayName };
    if (flow === "bootstrap") {
      startPath = "/api/v1/bootstrap/options";
      finishPath = "/api/v1/bootstrap/verify";
      request = { bootstrapToken: bearer, displayName };
    } else if (flow === "invite") {
      request.inviteToken = bearer;
    } else if (flow === "recovery") {
      startPath = "/api/v1/recovery/options";
      finishPath = "/api/v1/recovery/verify";
      request = { recoveryToken: bearer };
    } else if (flow === "add") {
      startPath = "/api/v1/me/passkeys/options";
      finishPath = "/api/v1/me/passkeys/verify";
      request = { label };
    }
    const started = await api(startPath, { method: "POST", body: request });
    const credential = await navigator.credentials.create({ publicKey: creationOptions(started.publicKey) });
    if (!credential) throw new Error("Passkey creation was cancelled.");
    return api(finishPath, { method: "POST", body: { ceremonyID: started.ceremonyID, credential: registrationCredential(credential) } });
  }

  async function signIn() {
    const control = byID("signin-button");
    control.disabled = true;
    control.textContent = "Waiting for passkey…";
    try {
      const started = await api("/api/v1/authentication/options", { method: "POST", body: {} });
      const credential = await navigator.credentials.get({ publicKey: requestOptions(started.publicKey) });
      if (!credential) throw new Error("Sign-in was cancelled.");
      await api("/api/v1/authentication/verify", { method: "POST", body: { ceremonyID: started.ceremonyID, credential: authenticationCredential(credential) } });
      state.user = await api("/api/v1/me");
      enterApplication();
      announce(`Signed in as ${state.user.displayName}.`);
    } catch (error) {
      announce(friendlyError(error, error.message || "Sign-in did not finish."), true);
    } finally {
      control.disabled = false;
      control.textContent = "Continue with a passkey";
    }
  }

  async function logout() {
    const suspendedTransfers = state.transfers.filter((transfer) => !["complete", "cancelled"].includes(transfer.state));
    for (const transfer of suspendedTransfers) {
      transfer.suspendedByLogout = true;
      if (transfer.controller) transfer.controller.abort();
      window.clearTimeout(state.transferRetryTimers.get(transfer.id));
      state.transferRetryTimers.delete(transfer.id);
      transitionTransfer(transfer, "paused", "Paused on this device.", "signed_out");
    }
    await Promise.all([
      ...suspendedTransfers.map(persistTransferItem),
      ...[...state.transferGroups.values()].map(persistTransferGroup),
    ]);
    try { await api("/api/v1/logout", { method: "POST", body: {} }); } catch { /* local transition still clears the UI */ }
    state.user = null;
    state.selected.clear();
    state.transfers = [];
    state.transferByID.clear();
    state.transferGroups.clear();
    state.transferProjection = [];
    state.transferQueueCursor = 0;
    state.transferLedgerOwner = "";
    state.transferPersistQueue.clear();
    window.clearTimeout(state.transferPersistTimer);
    state.transferPersistTimer = 0;
    window.clearInterval(state.transferFixtureTimer);
    state.transferFixtureTimer = 0;
    releaseViewerObjectURL();
    clearViewerPreviewCache();
    cleanupGridMedia(new Set());
    history.replaceState({}, "", "/");
    showOnly("auth-view");
    byID("account-actions").hidden = true;
    byID("signin-button").focus();
    announce("Signed out.");
  }

  function pathName(path) {
    if (path === "/") return "Files";
    const parts = path.split("/");
    return parts[parts.length - 1];
  }

  function joinPath(directory, name) {
    return directory === "/" ? `/${name}` : `${directory}/${name}`;
  }

  function parentPath(path) {
    const parts = path.split("/").filter(Boolean);
    parts.pop();
    return parts.length ? `/${parts.join("/")}` : "/";
  }

  function formatBytes(value) {
    if (!Number.isFinite(value) || value < 0) return "—";
    if (value < 1024) return `${value} B`;
    const units = ["KiB", "MiB", "GiB", "TiB"];
    let size = value / 1024;
    let unit = 0;
    while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit += 1; }
    return `${size.toFixed(size >= 10 ? 1 : 2)} ${units[unit]}`;
  }

  function formatRate(value) {
    return Number.isFinite(value) && value > 0 ? `${formatBytes(value)}/s` : "—/s";
  }

  function formatDuration(seconds) {
    if (!Number.isFinite(seconds) || seconds < 0) return "ETA —";
    const rounded = Math.max(1, Math.round(seconds));
    if (rounded < 60) return `${rounded}s remaining`;
    if (rounded < 3600) return `${Math.round(rounded / 60)}m remaining`;
    const hours = Math.floor(rounded / 3600);
    const minutes = Math.round((rounded % 3600) / 60);
    return `${hours}h${minutes ? ` ${minutes}m` : ""} remaining`;
  }

  function formatDate(value) {
    const date = new Date(value);
    if (Number.isNaN(date.valueOf())) return "Unknown";
    const now = new Date();
    const today = new Date(now.getFullYear(), now.getMonth(), now.getDate());
    const yesterday = new Date(today);
    yesterday.setDate(today.getDate() - 1);
    const sameCalendarDate = (candidate, reference) => candidate.getFullYear() === reference.getFullYear()
      && candidate.getMonth() === reference.getMonth()
      && candidate.getDate() === reference.getDate();
    const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
    const dayLabel = sameCalendarDate(date, today) ? "Today"
      : sameCalendarDate(date, yesterday) ? "Yesterday"
        : `${date.getDate()} ${months[date.getMonth()]} ${date.getFullYear()}`;
    const period = date.getHours() < 12 ? "am" : "pm";
    const hour = date.getHours() % 12 || 12;
    const minute = String(date.getMinutes()).padStart(2, "0");
    return `${dayLabel} at ${hour}:${minute} ${period}`;
  }

  function setRoute(route, push = true) {
    if (!state.user) return Promise.resolve();
    const paths = { drive: "/", trash: "/trash", settings: "/settings", admin: "/admin" };
    if (route === "admin" && !(state.user.roles || []).includes("admin")) route = "drive";
    if (route === "drive") syncFileBrowserAccess("owner");
    if (route === "trash") syncFileBrowserAccess("trash");
    if (route === "drive" || route === "trash") byID("browser-title").textContent = route === "trash" ? "Trash" : "Files";
    const routeURL = `${paths[route]}${state.safeTheme ? "?safe-theme=1" : ""}`;
    if (push && `${location.pathname}${location.search}` !== routeURL) history.pushState({ route }, "", routeURL);
    for (const name of Object.keys(paths)) byID(`${name}-view`).hidden = name !== route;
    document.querySelectorAll("[data-route]").forEach((link) => {
      if (link.closest(".app-tabs")) link.setAttribute("aria-current", link.dataset.route === route ? "page" : "false");
    });
    const heading = route === "drive" || route === "trash" ? byID("browser-title") : byID(`${route}-title`);
    if (heading) heading.focus({ preventScroll: true });
    if (route === "drive") return loadDirectory(state.currentDirectory);
    if (route === "trash") return loadTrash();
    if (route === "settings") return loadSettings();
    if (route === "admin") return loadAdmin();
    return Promise.resolve();
  }

  function routeFromPath() {
    if (location.pathname === "/trash") return "trash";
    if (location.pathname === "/settings") return "settings";
    if (location.pathname === "/admin") return "admin";
    return "drive";
  }

  function themePreferenceURL(dark) {
    const query = new URLSearchParams({ dark: String(dark) });
    if (state.safeTheme) query.set("safe-theme", "1");
    return `/api/v1/me/preferences/theme?${query.toString()}`;
  }

  function syncSafeThemeURL() {
    const current = new URL(location.href);
    if (state.safeTheme) current.searchParams.set("safe-theme", "1");
    else current.searchParams.delete("safe-theme");
    history.replaceState(history.state, "", `${current.pathname}${current.search}${current.hash}`);
  }

  function renderBreadcrumbs(targetID, directory, navigate) {
    const target = byID(targetID);
    target.replaceChildren();
    const root = button("Files", () => navigate("/"));
    root.setAttribute("aria-label", "Open root folder");
    target.append(root);
    let built = "";
    for (const part of directory.split("/").filter(Boolean)) {
      target.append(text("span", "/", "breadcrumb-separator"));
      built += `/${part}`;
      const destination = built;
      const item = button(part, () => navigate(destination));
      item.setAttribute("aria-label", `Open ${part}`);
      target.append(item);
    }
  }

  async function loadDirectory(directory, append = false) {
    if (append && state.directoryLoading) return;
    const request = state.directoryRequest + 1;
    state.directoryRequest = request;
    state.directoryLoading = true;
    state.currentDirectory = directory;
    if (!append) {
      cleanupGridMedia(new Set());
      state.previewStates.clear();
      state.entries = [];
      state.nextCursor = "";
      state.selected.clear();
      byID("list-presentation").scrollTop = 0;
      byID("media-grid").scrollTop = 0;
      updateSelection();
      showState("drive-state", "");
      renderFileLoadingItems();
    }
    renderBreadcrumbs("breadcrumbs", directory, (path) => loadDirectory(path));
    const [sort, order] = byID("file-sort").value.split(":");
    const cursor = append && state.nextCursor ? `&cursor=${encodeURIComponent(state.nextCursor)}` : "";
    try {
      const page = await api(`/api/v1/files?path=${encodeURIComponent(directory)}&limit=100&sort=${sort}&order=${order}${cursor}`);
      if (request !== state.directoryRequest) return;
      state.entries = append ? state.entries.concat(page.entries || []) : (page.entries || []);
      state.nextCursor = page.nextCursor || "";
      renderFiles();
      announce(`${page.entries.length} item${page.entries.length === 1 ? "" : "s"} loaded from ${pathName(directory)}.`);
    } catch (error) {
      if (request !== state.directoryRequest) return;
      if (append) announce(friendlyError(error, "More files could not be loaded."), true);
      else {
        showState("drive-state", friendlyError(error, "Files could not be loaded."), "error");
        byID("file-rows").replaceChildren();
        byID("media-grid-content").replaceChildren();
        finishListLoading("file-rows");
        finishListLoading("media-grid-content");
        announce(friendlyError(error), true);
      }
    } finally {
      if (request === state.directoryRequest) state.directoryLoading = false;
    }
  }

  function renderFiles() {
    const visible = filterLoadedEntries(state.entries);
    state.filteredEntries = visible;
    finishListLoading("file-rows");
    finishListLoading("media-grid-content");
    const gridEnabled = syncFilePresentation();
    if (gridEnabled) renderVirtualGrid(visible);
    else {
      cleanupGridMedia(new Set());
      renderVirtualList(visible);
    }
    const emptyMessages = {
      owner: "This folder is empty. Upload a file or create a folder.",
      public: "This shared folder is empty.",
      trash: "Trash is empty.",
    };
    const emptyMessage = emptyMessages[state.browserAccess] || emptyMessages.owner;
    showState("drive-state", visible.length ? "" : (state.entries.length ? "No loaded items match this filter." : emptyMessage));
    const nextCursor = state.browserAccess === "public" ? state.publicCursor : state.browserAccess === "trash" ? state.trashCursor : state.nextCursor;
    byID("next-page").hidden = !nextCursor;
    if (state.browserAccess === "owner") {
      const selectedVisible = visible.filter((entry) => state.selected.has(entry.path)).length;
      byID("select-all").checked = visible.length > 0 && selectedVisible === visible.length;
      byID("select-all").indeterminate = selectedVisible > 0 && selectedVisible < visible.length;
    }
  }

  function listSpacerRow(height, columns = 6) {
    const row = document.createElement("tr");
    row.className = "list-spacer";
    row.setAttribute("aria-hidden", "true");
    const cell = document.createElement("td");
    cell.colSpan = columns;
    const spacer = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    spacer.setAttribute("width", "1");
    spacer.setAttribute("height", String(Math.max(0, Math.round(height))));
    spacer.setAttribute("aria-hidden", "true");
    cell.append(spacer);
    row.append(cell);
    return row;
  }

  function renderVirtualList(entries) {
    const rows = byID("file-rows");
    const scroller = byID("list-presentation");
    const viewportHeight = Math.max(listRowHeight, scroller.clientHeight - listRowHeight);
    const firstVisible = Math.max(0, Math.floor(Math.max(0, scroller.scrollTop - listRowHeight) / listRowHeight));
    const start = Math.max(0, firstVisible - listOverscanRows);
    const end = Math.min(entries.length, firstVisible + Math.ceil(viewportHeight / listRowHeight) + listOverscanRows);
    const fragment = document.createDocumentFragment();
    if (start > 0) fragment.append(listSpacerRow(start * listRowHeight));
    for (let index = start; index < end; index += 1) fragment.append(fileRow(entries[index]));
    if (end < entries.length) fragment.append(listSpacerRow((entries.length - end) * listRowHeight));
    rows.replaceChildren(fragment);
    rows.dataset.itemCount = String(entries.length);
    rows.dataset.renderedCount = String(end - start);
  }

  function emptyTableRow(message, columns) {
    const row = document.createElement("tr");
    row.className = "empty-table-row";
    const cell = document.createElement("td");
    cell.colSpan = columns;
    cell.textContent = message;
    row.append(cell);
    return row;
  }

  function renderVirtualSettingsTable(scrollerID, targetID, entries, columns, rowFactory, emptyMessage, force = false) {
    const scroller = byID(scrollerID);
    const rows = byID(targetID);
    const fragment = document.createDocumentFragment();
    if (!entries.length) {
      if (!force && rows.dataset.windowKey === "empty") return;
      fragment.append(emptyTableRow(emptyMessage, columns));
      rows.replaceChildren(fragment);
      rows.dataset.itemCount = "0";
      rows.dataset.renderedCount = "0";
      rows.dataset.windowKey = "empty";
      finishListLoading(targetID);
      return;
    }
    const headingHeight = scroller.querySelector("thead").getBoundingClientRect().height || settingsRowHeight;
    const viewportHeight = Math.max(settingsRowHeight, scroller.clientHeight - headingHeight);
    const firstVisible = Math.max(0, Math.floor(scroller.scrollTop / settingsRowHeight));
    const start = Math.max(0, firstVisible - settingsOverscanRows);
    const end = Math.min(entries.length, firstVisible + Math.ceil(viewportHeight / settingsRowHeight) + settingsOverscanRows);
    const windowKey = `${entries.length}:${start}:${end}`;
    if (!force && rows.dataset.windowKey === windowKey) return;
    if (start > 0) fragment.append(listSpacerRow(start * settingsRowHeight, columns));
    for (let index = start; index < end; index += 1) fragment.append(rowFactory(entries[index]));
    if (end < entries.length) fragment.append(listSpacerRow((entries.length - end) * settingsRowHeight, columns));
    rows.replaceChildren(fragment);
    rows.dataset.itemCount = String(entries.length);
    rows.dataset.renderedCount = String(end - start);
    rows.dataset.windowKey = windowKey;
    finishListLoading(targetID);
  }

  function filterLoadedEntries(entries) {
    const query = byID("file-filter").value.trim().toLocaleLowerCase();
    const kind = byID("filter-kind").value;
    const media = byID("filter-media").value;
    const previewState = state.browserAccess === "owner" ? byID("filter-preview").value : "";
    const minimumSize = Number.parseInt(byID("filter-min-size").value, 10);
    const maximumSize = Number.parseInt(byID("filter-max-size").value, 10);
    const modifiedAfter = byID("filter-modified-after").value ? new Date(`${byID("filter-modified-after").value}T00:00:00`).valueOf() : Number.NEGATIVE_INFINITY;
    const modifiedBefore = byID("filter-modified-before").value ? new Date(`${byID("filter-modified-before").value}T23:59:59.999`).valueOf() : Number.POSITIVE_INFINITY;
    const filtered = entries.filter((entry) => {
      const changed = new Date(entry.modifiedAt).valueOf();
      const knownPreviewState = state.previewStates.get(entry.path);
      return (!query || entry.name.toLocaleLowerCase().includes(query)) &&
        (!kind || entry.kind === kind) &&
        (!media || mediaCategory(entry) === media) &&
        (!Number.isFinite(minimumSize) || entry.kind === "directory" || entry.size >= minimumSize) &&
        (!Number.isFinite(maximumSize) || entry.kind === "directory" || entry.size <= maximumSize) &&
        (!Number.isFinite(changed) || (changed >= modifiedAfter && changed <= modifiedBefore)) &&
        (!previewState || (knownPreviewState ? knownPreviewState.state : "missing") === previewState);
    });
    const [sort, order] = byID("file-sort").value.split(":");
    const direction = order === "desc" ? -1 : 1;
    const compare = (left, right) => {
      if (sort === "modified") return direction * (new Date(left.modifiedAt).valueOf() - new Date(right.modifiedAt).valueOf());
      if (sort === "size") return direction * ((left.size || 0) - (right.size || 0));
      if (sort === "kind") return direction * (left.kind.localeCompare(right.kind) || left.name.localeCompare(right.name));
      return direction * left.name.localeCompare(right.name);
    };
    return filtered.map((entry, index) => ({ entry, index })).sort((left, right) => compare(left.entry, right.entry) || left.index - right.index).map((item) => item.entry);
  }

  function mediaCategory(entry) {
    if (entry.kind === "directory") return "folder";
    const mediaType = (entry.mediaType || "").toLocaleLowerCase();
    const extension = (entry.name.split(".").pop() || "").toLocaleLowerCase();
    if (mediaType.startsWith("image/")) return "image";
    if (mediaType.startsWith("video/")) return "video";
    if (mediaType === "application/pdf" || extension === "pdf") return "pdf";
    if (mediaType.startsWith("audio/")) return "audio";
    if (mediaType.startsWith("text/") || ["application/rtf", "application/msword", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"].includes(mediaType)) return "document";
    if (["application/zip", "application/gzip", "application/x-tar", "application/x-7z-compressed", "application/vnd.rar"].includes(mediaType) || ["zip", "gz", "tar", "7z", "rar"].includes(extension)) return "archive";
    return "unknown";
  }

  function previewEligible(entry) {
    return Boolean(state.config && state.config.previewConfigured && entry.kind === "file" &&
      Array.isArray(state.config.previewFormats) && state.config.previewFormats.includes(mediaCategory(entry)));
  }

  function fileTypeIcon(entry, className = "file-icon") {
    const category = mediaCategory(entry);
    const slot = category === "folder" ? "icon.folder" : `icon.file.${category}`;
    const descriptor = state.themeAssets[slot] || state.themeAssets[entry.kind === "directory" ? "icon.folder" : "icon.file"];
    const wrapper = document.createElement("span");
    wrapper.className = className;
    wrapper.setAttribute("aria-hidden", "true");
    if (descriptor && descriptor.url) {
      const image = document.createElement("img");
      image.src = descriptor.url;
      image.alt = "";
      image.width = descriptor.width || 32;
      image.height = descriptor.height || 32;
      wrapper.append(image);
    } else {
      wrapper.textContent = category === "folder" ? "▰" : category === "image" ? "▧" : category === "video" ? "▶" : category === "pdf" ? "PDF" : category === "audio" ? "♪" : category === "archive" ? "▦" : "▤";
    }
    return wrapper;
  }

  function renderVirtualGrid(entries) {
    const scroller = byID("media-grid");
    const content = byID("media-grid-content");
    if (state.gridObserver) state.gridObserver.disconnect();
    const rootMetrics = getComputedStyle(document.documentElement);
    const thumbnailSize = Number.parseFloat(rootMetrics.getPropertyValue("--efs-metric-thumbnailSize")) || 96;
    const tileMinimum = Math.max(80, thumbnailSize);
    const columns = Math.max(1, Math.floor(Math.max(scroller.clientWidth, 280) / tileMinimum));
    const rowHeight = Math.max(tileMinimum, scroller.clientWidth / columns);
    const totalRows = Math.ceil(entries.length / columns);
    const viewportHeight = Math.max(scroller.clientHeight, 420);
    const firstVisible = Math.floor(scroller.scrollTop / rowHeight);
    const lastVisible = Math.ceil((scroller.scrollTop + viewportHeight) / rowHeight);
    const firstRow = Math.max(0, firstVisible - gridOverscanRows);
    const lastRow = Math.min(totalRows, lastVisible + gridOverscanRows);
    const firstIndex = firstRow * columns;
    const lastIndex = Math.min(entries.length, lastRow * columns);
    const activePaths = new Set(entries.slice(firstIndex, lastIndex).map((entry) => entry.path));
    cleanupGridMedia(activePaths);
    content.replaceChildren();
    content.dataset.itemCount = String(entries.length);
    const layer = document.createElement("div");
    layer.className = "media-grid-layer";
    for (let index = firstIndex; index < lastIndex; index += 1) layer.append(mediaTile(entries[index], index, columns));
    content.append(mediaGridSpacer(firstRow * rowHeight), layer, mediaGridSpacer((totalRows - lastRow) * rowHeight));
    scroller.setAttribute("aria-rowcount", String(totalRows));
    scroller.setAttribute("aria-colcount", String(columns));
    const observe = (frame) => queueGridPreview(frame.entry, frame);
    if ("IntersectionObserver" in window) {
      state.gridObserver = new IntersectionObserver((records) => {
        for (const record of records) if (record.isIntersecting) observe(record.target);
      }, { root: scroller, rootMargin: `${rowHeight * gridOverscanRows}px 0px`, threshold: 0.01 });
      layer.querySelectorAll(".media-frame[data-preview-candidate='true']").forEach((frame) => state.gridObserver.observe(frame));
    } else {
      layer.querySelectorAll(".media-frame[data-preview-candidate='true']").forEach(observe);
    }
  }

  function mediaGridSpacer(height) {
    const spacer = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    spacer.classList.add("media-grid-spacer");
    spacer.setAttribute("width", "1");
    spacer.setAttribute("height", String(Math.max(0, Math.round(height))));
    spacer.setAttribute("aria-hidden", "true");
    spacer.setAttribute("focusable", "false");
    return spacer;
  }

  function mediaTile(entry, index, columns) {
    const tile = document.createElement("article");
    const ownerAccess = state.browserAccess === "owner";
    const trashAccess = state.browserAccess === "trash";
    tile.className = `media-tile${ownerAccess && state.selected.has(entry.path) ? " selected" : ""}`;
    tile.setAttribute("role", "gridcell");
    tile.setAttribute("aria-rowindex", String(Math.floor(index / columns) + 1));
    tile.setAttribute("aria-colindex", String(index % columns + 1));
    if (trashAccess) tile.setAttribute("aria-label", `Trashed ${entry.name}`);
    const checkbox = document.createElement("input");
    checkbox.className = "owner-only";
    checkbox.type = "checkbox";
    checkbox.checked = state.selected.has(entry.path);
    checkbox.setAttribute("aria-label", `Select ${entry.name}`);
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) state.selected.set(entry.path, entry); else state.selected.delete(entry.path);
      renderFiles(); updateSelection();
    });
    const open = document.createElement(trashAccess ? "div" : "button");
    if (!trashAccess) open.type = "button";
    open.className = "media-tile-open";
    if (!trashAccess) open.setAttribute("aria-label", `${entry.kind === "directory" ? "Open folder" : "View file"} ${entry.name}`);
    const frame = document.createElement("span");
    frame.className = "media-frame";
    frame.entry = entry;
    frame.dataset.path = entry.path;
    if (ownerAccess && previewEligible(entry)) frame.dataset.previewCandidate = "true";
    const objectURL = state.previewObjectURLs.get(entry.path);
    if (objectURL) setPreviewImage(frame, entry, objectURL, state.previewStates.get(entry.path));
    else frame.append(fileTypeIcon(entry, "media-fallback-icon"));
    open.append(frame, text("strong", entry.name, "media-tile-name"), text("span", entry.kind === "directory" ? "" : formatBytes(entry.size), "media-tile-meta"));
    if (!trashAccess) open.addEventListener("click", () => openBrowserEntry(entry));
    tile.append(checkbox, open);
    if (trashAccess) tile.append(trashActionGroup(entry.trash, "trash-media-actions"));
    return tile;
  }

  function previewVariant(renderedCSSPixels) {
    const resolutions = state.config && Array.isArray(state.config.previewResolutions) ? state.config.previewResolutions : [256, 512, 1600];
    const target = Math.max(1, renderedCSSPixels * (window.devicePixelRatio || 1));
    return resolutions.find((value) => value >= target) || resolutions[resolutions.length - 1] || 256;
  }

  function queueGridPreview(entry, frame) {
    const known = state.previewStates.get(entry.path);
    if (!previewEligible(entry) || (known && known.state !== "ready" && known.state !== "generating") || state.previewQueued.has(entry.path) || state.previewControllers.has(entry.path) || state.previewObjectURLs.has(entry.path)) return;
    state.previewQueued.add(entry.path);
    state.previewQueue.push({ entry, frame });
    pumpPreviewQueue();
  }

  function pumpPreviewQueue() {
    const maximum = Math.max(1, Math.min(8, state.config && state.config.previewMaxConcurrency ? state.config.previewMaxConcurrency : 2));
    while (state.previewActive < maximum && state.previewQueue.length) {
      const queued = state.previewQueue.shift();
      state.previewQueued.delete(queued.entry.path);
      if (!queued.frame.isConnected) continue;
      state.previewActive += 1;
      const controller = new AbortController();
      state.previewControllers.set(queued.entry.path, controller);
      resolveGridPreview(queued.entry, queued.frame, controller).finally(() => {
        if (state.previewControllers.get(queued.entry.path) === controller) state.previewControllers.delete(queued.entry.path);
        state.previewActive -= 1;
        pumpPreviewQueue();
      });
    }
  }

  async function resolveGridPreview(entry, frame, controller) {
    try {
      const variant = previewVariant(frame.getBoundingClientRect().width || 96);
      const response = await api("/api/v1/previews/resolve", { method: "POST", body: { items: [{ path: entry.path, version: entry.version, variant }] }, signal: controller.signal });
      const result = response.items[0];
      if (controller.signal.aborted) return;
      state.previewStates.set(entry.path, result);
      let currentFrame = connectedGridFrame(entry.path) || frame;
      if (currentFrame.isConnected) currentFrame.dataset.previewState = result.state;
      if (result.state !== "ready" || !result.capability) {
        if (result.state === "generating") scheduleGridPreviewRetry(entry, currentFrame);
        else clearGridPreviewRetry(entry.path);
        if (byID("filter-preview").value) renderFiles();
        return;
      }
      clearGridPreviewRetry(entry.path);
      const artifactResponse = await fetch(result.capability.url, { method: result.capability.method || "GET", headers: result.capability.headers || {}, signal: controller.signal, credentials: "omit" });
      const blob = await validatedPreviewBlob(artifactResponse, result.artifact);
      if (controller.signal.aborted) return;
      const objectURL = URL.createObjectURL(blob);
      const previous = state.previewObjectURLs.get(entry.path);
      if (previous) URL.revokeObjectURL(previous);
      state.previewObjectURLs.set(entry.path, objectURL);
      currentFrame = connectedGridFrame(entry.path) || frame;
      if (currentFrame.isConnected) setPreviewImage(currentFrame, entry, objectURL, result);
    } catch (error) {
      if (error.name !== "AbortError") {
        const result = { state: "unavailable" };
        state.previewStates.set(entry.path, result);
        const currentFrame = connectedGridFrame(entry.path) || frame;
        if (currentFrame.isConnected) currentFrame.dataset.previewState = result.state;
        clearGridPreviewRetry(entry.path);
      }
    }
  }

  async function validatedPreviewBlob(response, artifact) {
    if (!response.ok || response.headers.get("Content-Type") !== "image/webp" || !artifact || artifact.contentType !== "image/webp" || !Number.isSafeInteger(artifact.size) || artifact.size < 12) throw new Error("Invalid preview artifact response");
    const data = await response.arrayBuffer();
    const bytes = new Uint8Array(data);
    if (bytes.length !== artifact.size) throw new Error("Invalid preview artifact checksum");
    const expected = decodeSHA256(artifact.sha256);
    const actual = new Uint8Array(await crypto.subtle.digest("SHA-256", data));
    let mismatch = 0;
    for (let index = 0; index < actual.length; index += 1) mismatch |= expected[index] ^ actual[index];
    if (mismatch !== 0) throw new Error("Invalid preview artifact checksum");
    if (bytes.length < 12 || bytes[0] !== 0x52 || bytes[1] !== 0x49 || bytes[2] !== 0x46 || bytes[3] !== 0x46 || bytes[8] !== 0x57 || bytes[9] !== 0x45 || bytes[10] !== 0x42 || bytes[11] !== 0x50) {
      throw new Error("Invalid preview artifact body");
    }
    return new Blob([data], { type: "image/webp" });
  }

  function decodeSHA256(value) {
    if (typeof value !== "string" || !/^[A-Za-z0-9_-]{43}$/.test(value)) throw new Error("Invalid preview artifact checksum");
    const decoded = atob(value.replace(/-/g, "+").replace(/_/g, "/") + "=");
    if (decoded.length !== 32) throw new Error("Invalid preview artifact checksum");
    return Uint8Array.from(decoded, (character) => character.charCodeAt(0));
  }

  function scheduleGridPreviewRetry(entry, frame) {
    clearGridPreviewRetry(entry.path, false);
    const attempt = state.previewRetryAttempts.get(entry.path) || 0;
    if (attempt >= maximumPreviewPolls) {
      const failed = { state: "failed" };
      state.previewStates.set(entry.path, failed);
      if (frame && frame.isConnected) frame.dataset.previewState = failed.state;
      state.previewRetryAttempts.delete(entry.path);
      return;
    }
    state.previewRetryAttempts.set(entry.path, attempt + 1);
    const delay = previewRetryDelays[Math.min(attempt, previewRetryDelays.length - 1)];
    const timer = window.setTimeout(() => {
      state.previewRetryTimers.delete(entry.path);
      const currentFrame = connectedGridFrame(entry.path);
      if (!currentFrame || !currentFrame.isConnected || state.previewStates.get(entry.path)?.state !== "generating") return;
      queueGridPreview(entry, currentFrame);
    }, delay);
    state.previewRetryTimers.set(entry.path, timer);
  }

  function clearGridPreviewRetry(path, resetAttempts = true) {
    const timer = state.previewRetryTimers.get(path);
    if (timer) window.clearTimeout(timer);
    state.previewRetryTimers.delete(path);
    if (resetAttempts) state.previewRetryAttempts.delete(path);
  }

  function connectedGridFrame(path) {
    return Array.from(byID("media-grid-content").querySelectorAll(".media-frame")).find((candidate) => candidate.dataset.path === path) || null;
  }

  function setPreviewImage(frame, entry, objectURL, result) {
    const image = document.createElement("img");
    image.addEventListener("load", () => {
      if (image.isConnected) frame.dataset.previewLoaded = "true";
    }, { once: true });
    image.addEventListener("error", () => {
      if (!image.isConnected) return;
      if (state.previewObjectURLs.get(entry.path) === objectURL) {
        URL.revokeObjectURL(objectURL);
        state.previewObjectURLs.delete(entry.path);
      }
      state.previewStates.set(entry.path, { state: "unavailable" });
      frame.dataset.previewState = "unavailable";
      frame.removeAttribute("data-preview-loaded");
      frame.replaceChildren(fileTypeIcon(entry, "media-fallback-icon"));
    }, { once: true });
    image.src = objectURL;
    image.alt = `Preview of ${entry.name}`;
    image.width = result && result.artifact ? result.artifact.width : 1;
    image.height = result && result.artifact ? result.artifact.height : 1;
    image.decoding = "async";
    frame.replaceChildren(image);
  }

  function cleanupGridMedia(activePaths) {
    if (state.gridObserver) { state.gridObserver.disconnect(); state.gridObserver = null; }
    for (const [path, controller] of state.previewControllers) {
      if (!activePaths.has(path)) {
        controller.abort();
        state.previewControllers.delete(path);
        if (state.previewStates.get(path) && state.previewStates.get(path).state === "ready") state.previewStates.delete(path);
      }
    }
    state.previewQueue = state.previewQueue.filter((item) => activePaths.has(item.entry.path));
    state.previewQueued = new Set(state.previewQueue.map((item) => item.entry.path));
    for (const [path, objectURL] of state.previewObjectURLs) {
      if (!activePaths.has(path)) {
        URL.revokeObjectURL(objectURL);
        state.previewObjectURLs.delete(path);
        if (state.previewStates.get(path) && state.previewStates.get(path).state === "ready") state.previewStates.delete(path);
      }
    }
    for (const path of state.previewRetryTimers.keys()) {
      if (!activePaths.has(path)) clearGridPreviewRetry(path);
    }
  }

  function fileRow(entry) {
    const row = document.createElement("tr");
    const ownerAccess = state.browserAccess === "owner";
    const trashAccess = state.browserAccess === "trash";
    if (ownerAccess && state.selected.has(entry.path)) row.className = "selected";
    const selectCell = document.createElement("td");
    selectCell.className = "owner-only";
    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.checked = state.selected.has(entry.path);
    checkbox.setAttribute("aria-label", `Select ${entry.name}`);
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) state.selected.set(entry.path, entry); else state.selected.delete(entry.path);
      renderFiles();
      updateSelection();
    });
    selectCell.append(checkbox);
    const nameCell = document.createElement("td");
    if (trashAccess) {
      const name = text("span", entry.name, "file-name trash-file-name");
      name.prepend(fileTypeIcon(entry));
      nameCell.append(name);
    } else {
      const open = button(entry.name, () => openBrowserEntry(entry), "file-name");
      open.prepend(fileTypeIcon(entry));
      open.setAttribute("aria-label", `${entry.kind === "directory" ? "Open folder" : "Preview file"} ${entry.name}`);
      nameCell.append(open);
    }
    const sizeCell = text("td", entry.kind === "directory" ? "" : formatBytes(entry.size));
    const mediaTypeCell = text("td", entry.kind === "directory" ? "—" : entry.mediaType || "application/octet-stream");
    const dateCell = text("td", formatDate(entry.modifiedAt));
    const actionCell = document.createElement("td");
    actionCell.className = "table-icon-action-cell";
    const actions = document.createElement("div");
    actions.className = "row-actions file-row-actions";
    if (!trashAccess && entry.kind === "file") actions.append(iconButton("download", `Download ${entry.name}`, () => download(entry, ownerAccess ? "" : state.publicToken), "file-row-action", "Download"));
    if (ownerAccess) {
      actions.append(
        iconButton("share-3", `Create public share for ${entry.name}`, () => createShare(entry), "file-row-action", "Share"),
        iconButton("copy", `Copy ${entry.name}`, () => copyMove(false, [entry], false), "file-row-action", "Copy"),
        iconButton("folder-symlink", `Move ${entry.name}`, () => copyMove(true, [entry], false), "file-row-action", "Move"),
        iconButton("trash", `Move ${entry.name} to trash`, () => trashEntries([entry], false), "file-row-action danger", "Move to trash"),
      );
    }
    if (trashAccess) actions.append(...trashActionGroup(entry.trash).children);
    actionCell.append(actions);
    row.append(selectCell, nameCell, sizeCell, mediaTypeCell, dateCell, actionCell);
    return row;
  }

  function trashActionGroup(item, className = "") {
    const actions = document.createElement("div");
    actions.className = `row-actions${className ? ` ${className}` : ""}`;
    actions.append(
      iconButton("restore", "Restore", () => restoreTrash(item), "file-row-action"),
      iconButton("trash-x", "Delete Permanently", () => deleteTrash(item), "file-row-action danger"),
    );
    return actions;
  }

  function openBrowserEntry(entry) {
    if (entry.kind === "directory") {
      if (state.browserAccess === "public") {
        state.publicPath = entry.path;
        loadPublicShare();
      } else loadDirectory(entry.path);
      return;
    }
    preview(entry, state.browserAccess === "public" ? state.publicToken : "");
  }

  function updateSelection() {
    const count = state.selected.size;
    byID("selection-bar").hidden = count === 0;
    if (count > 0) byID("app").dataset.selectionActive = "true";
    else delete byID("app").dataset.selectionActive;
    byID("selection-count").textContent = `${count} selected`;
    byID("download-selected").disabled = count !== 1 || [...state.selected.values()][0].kind !== "file";
    byID("share-selected").disabled = count !== 1;
  }

  async function createFolder() {
    const result = await ask({ title: "New folder", description: `Create a folder inside ${state.currentDirectory}.`, confirm: "Create folder", fields: [{ id: "folder-name", label: "Folder name", required: true, maxLength: 255 }], conflictField: true });
    if (!result) return;
    try {
      await api("/api/v1/directories", { method: "POST", body: { path: joinPath(state.currentDirectory, result["folder-name"]), conflict: result.conflict } });
      announce(`Folder ${result["folder-name"]} created.`);
      await loadDirectory(state.currentDirectory);
    } catch (error) { announce(friendlyError(error), true); }
  }

  function transferFileSize(transfer) {
    return Number.isSafeInteger(transfer.size) ? transfer.size : transfer.file instanceof File ? transfer.file.size : 0;
  }

  function transferSourceAvailable(transfer) {
    return transfer.file instanceof File || transfer.fixture === true;
  }

  function transferMediaType(transfer) {
    if (transfer.mediaType) return transfer.mediaType;
    if (transfer.file instanceof File) return uploadMediaType(transfer.file, transfer.name);
    return "application/octet-stream";
  }

  function ledgerRequest(request) {
    return new Promise((resolve, reject) => {
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error || new Error("Transfer ledger request failed."));
    });
  }

  function ledgerTransactionDone(transaction) {
    return new Promise((resolve, reject) => {
      transaction.oncomplete = () => resolve();
      transaction.onabort = () => reject(transaction.error || new Error("Transfer ledger transaction was aborted."));
      transaction.onerror = () => reject(transaction.error || new Error("Transfer ledger transaction failed."));
    });
  }

  async function openTransferLedger() {
    if (state.transferLedger) return state.transferLedger;
    if (!("indexedDB" in window)) return null;
    const request = indexedDB.open(transferLedgerDatabaseName, transferLedgerVersion);
    request.onupgradeneeded = () => {
      const database = request.result;
      for (const storeName of ["groups", "items", "sources"]) {
        const store = database.objectStoreNames.contains(storeName)
          ? request.transaction.objectStore(storeName)
          : database.createObjectStore(storeName, { keyPath: "key" });
        if (!store.indexNames.contains("ownerID")) store.createIndex("ownerID", "ownerID", { unique: false });
      }
    };
    state.transferLedger = await ledgerRequest(request);
    state.transferLedger.addEventListener("versionchange", () => {
      state.transferLedger.close();
      state.transferLedger = null;
    });
    return state.transferLedger;
  }

  function warnTransferLedger(message) {
    if (state.transferLedgerWarningShown) return;
    state.transferLedgerWarningShown = true;
    showToast(message, "warning");
  }

  function transferLedgerKey(ownerID, id) {
    return `${ownerID}:${id}`;
  }

  function transferLedgerItem(transfer) {
    const ownerID = state.transferLedgerOwner;
    return {
      key: transferLedgerKey(ownerID, transfer.id), ownerID, id: transfer.id,
      groupID: transfer.groupID || "", name: transfer.name, directory: transfer.directory,
      baseDirectory: transfer.baseDirectory, relativeDirectory: transfer.relativeDirectory || "",
      relativePath: transfer.relativePath, size: transferFileSize(transfer), mediaType: transferMediaType(transfer),
      lastModified: Number.isSafeInteger(transfer.lastModified) ? transfer.lastModified : 0,
      state: transfer.state, confirmed: Math.max(0, transfer.confirmed || 0), uploadID: transfer.uploadID || "",
      retryCount: Math.max(0, transfer.retryCount || 0), nextRetryAt: Math.max(0, transfer.nextRetryAt || 0),
      errorCode: String(transfer.errorCode || "").slice(0, 80), error: String(transfer.error || "").slice(0, 240),
      createdAt: transfer.createdAt || Date.now(), updatedAt: Date.now(),
    };
  }

  function transferLedgerGroup(group) {
    const ownerID = state.transferLedgerOwner;
    return {
      key: transferLedgerKey(ownerID, group.id), ownerID, id: group.id, name: group.name,
      baseDirectory: group.baseDirectory, directories: [...(group.directories || [])], transferIDs: [...group.transferIDs],
      totalSize: group.totalSize || 0, state: group.state, error: String(group.error || "").slice(0, 240),
      cancelled: Boolean(group.cancelled), discoveryDone: group.discoveryDone !== false,
      createdAt: group.createdAt || Date.now(), updatedAt: Date.now(),
    };
  }

  async function persistTransferItem(transfer) {
    if (transfer.fixture || !state.transferLedgerOwner) return;
    try {
      const database = await openTransferLedger();
      if (!database) return;
      const transaction = database.transaction("items", "readwrite");
      transaction.objectStore("items").put(transferLedgerItem(transfer));
      await ledgerTransactionDone(transaction);
    } catch {
      warnTransferLedger("Transfer history could not be saved on this device.");
    }
  }

  async function persistTransferGroup(group) {
    if (group.fixture || !state.transferLedgerOwner) return;
    try {
      const database = await openTransferLedger();
      if (!database) return;
      const transaction = database.transaction("groups", "readwrite");
      transaction.objectStore("groups").put(transferLedgerGroup(group));
      await ledgerTransactionDone(transaction);
    } catch {
      warnTransferLedger("Transfer history could not be saved on this device.");
    }
  }

  function queueTransferPersistence(transfer) {
    if (transfer.fixture || !state.transferLedgerOwner) return;
    state.transferPersistQueue.set(transfer.id, transfer);
    if (state.transferPersistTimer) return;
    state.transferPersistTimer = window.setTimeout(async () => {
      state.transferPersistTimer = 0;
      const pending = [...state.transferPersistQueue.values()];
      state.transferPersistQueue.clear();
      await Promise.all(pending.map(persistTransferItem));
    }, 500);
  }

  async function persistTransferSource(transfer, handle) {
    if (!handle || transfer.fixture || !state.transferLedgerOwner) return;
    try {
      const database = await openTransferLedger();
      if (!database) return;
      const transaction = database.transaction("sources", "readwrite");
      transaction.objectStore("sources").put({ key: transferLedgerKey(state.transferLedgerOwner, transfer.id), ownerID: state.transferLedgerOwner, handle });
      await ledgerTransactionDone(transaction);
    } catch {
      // Some browsers expose handles without allowing them to be cloned. The
      // ledger remains useful and the source can be reconnected explicitly.
    }
  }

  async function transferLedgerRecords(storeName, ownerID) {
    const database = await openTransferLedger();
    if (!database) return [];
    const transaction = database.transaction(storeName, "readonly");
    const records = await ledgerRequest(transaction.objectStore(storeName).index("ownerID").getAll(IDBKeyRange.only(ownerID)));
    await ledgerTransactionDone(transaction);
    return records;
  }

  async function restoreTransferSource(transfer, sourceRecords) {
    const source = sourceRecords.get(transfer.id);
    if (!source || !source.handle || typeof source.handle.queryPermission !== "function") return false;
    transfer.sourceHandle = source.handle;
    try {
      if (await source.handle.queryPermission({ mode: "read" }) !== "granted") return false;
      const file = await source.handle.getFile();
      if (file.size !== transferFileSize(transfer) || (transfer.lastModified && file.lastModified !== transfer.lastModified)) return false;
      transfer.file = file;
      return true;
    } catch {
      return false;
    }
  }

  async function reconcileRestoredTransfer(transfer) {
    if (["complete", "cancelled"].includes(transfer.state)) return;
    if (transfer.uploadID) {
      try {
        const status = await api(`/api/v1/uploads/${encodeURIComponent(transfer.uploadID)}`);
        transfer.confirmed = Math.max(transfer.confirmed, Number(status.confirmedOffset) || 0);
        if (status.state === "completed") {
          transfer.state = "complete";
          transfer.confirmed = transferFileSize(transfer);
          transfer.error = "";
          return;
        }
        if (status.state === "aborted" || status.state === "expired") {
          transfer.state = "failed";
          transfer.errorCode = status.state;
          transfer.error = status.state === "expired" ? "Upload expired. Reconnect the source to start again." : "Upload was cancelled.";
          return;
        }
      } catch (error) {
        if (!(error instanceof APIError) || error.status !== 404) {
          transfer.state = navigator.onLine ? "retry-wait" : "paused";
          transfer.error = "Waiting to reconcile upload state.";
          return;
        }
      }
    }
    if (!(transfer.file instanceof File)) {
      transfer.state = "needs-source";
      transfer.errorCode = "source_required";
      transfer.error = "Reconnect the source to continue.";
    } else if (transfer.state === "retry-wait" && transfer.nextRetryAt > Date.now()) {
      state.transferRetryTimers.set(transfer.id, window.setTimeout(() => {
        state.transferRetryTimers.delete(transfer.id);
        if (transfer.state !== "retry-wait") return;
        transitionTransfer(transfer, "queued");
        renderTransfers();
        pumpTransfers();
      }, transfer.nextRetryAt - Date.now()));
    } else if (transfer.state === "paused" && navigator.onLine) {
      transfer.state = "queued";
      transfer.error = "";
    } else if (!["failed", "retry-wait"].includes(transfer.state)) {
      transfer.state = "queued";
      transfer.error = "";
    }
  }

  async function restoreTransferLedger() {
    if (!state.user || !state.user.userID || (state.config && state.config.localFixture && new URLSearchParams(location.search).get("fixture") === "transfers")) return;
    state.transferLedgerOwner = state.user.userID;
    try {
      const [itemRecords, groupRecords, storedSources] = await Promise.all([
        transferLedgerRecords("items", state.transferLedgerOwner),
        transferLedgerRecords("groups", state.transferLedgerOwner),
        transferLedgerRecords("sources", state.transferLedgerOwner),
      ]);
      const sourceRecords = new Map(storedSources.map((source) => [source.key.slice(source.key.indexOf(":") + 1), source]));
      state.transfers = itemRecords.map((record) => ({
        ...record, file: null, controller: null, speedBps: 0, lastProgressAt: 0,
        lastProgressBytes: record.confirmed || 0, recoveryFailures: 0,
      }));
      state.transferByID = new Map(state.transfers.map((transfer) => [transfer.id, transfer]));
      state.transferGroups = new Map(groupRecords.map((record) => [record.id, {
        ...record, refreshed: record.state === "complete", failureAnnounced: record.state === "failed",
      }]));
      let nextIndex = 0;
      const workers = Array.from({ length: Math.min(4, state.transfers.length) }, async () => {
        while (nextIndex < state.transfers.length) {
          const transfer = state.transfers[nextIndex];
          nextIndex += 1;
          await restoreTransferSource(transfer, sourceRecords);
          await reconcileRestoredTransfer(transfer);
          queueTransferPersistence(transfer);
        }
      });
      await Promise.all(workers);
      for (const group of state.transferGroups.values()) updateTransferGroup(group.id, false);
      if (state.transfers.length) {
        renderTransfers();
        setTransferSheetOpen(false);
        pumpTransfers();
      }
    } catch {
      warnTransferLedger("Transfer history could not be restored on this device.");
    }
  }

  async function deleteTransferLedgerEntries(itemIDs, groupIDs) {
    if (!state.transferLedgerOwner) return;
    const database = await openTransferLedger();
    if (!database) return;
    const transaction = database.transaction(["items", "groups", "sources"], "readwrite");
    for (const id of itemIDs) {
      const key = transferLedgerKey(state.transferLedgerOwner, id);
      transaction.objectStore("items").delete(key);
      transaction.objectStore("sources").delete(key);
    }
    for (const id of groupIDs) transaction.objectStore("groups").delete(transferLedgerKey(state.transferLedgerOwner, id));
    await ledgerTransactionDone(transaction);
  }

  function validUploadComponents(relative) {
    if (typeof relative !== "string" || !relative) return null;
    const components = relative.split("/");
    if (!components.length || components.some((part) => !part || part === "." || part === ".." || part.includes("\\") || part.includes("\u0000"))) return null;
    return components;
  }

  function setTransferSheetOpen(open, focus = false) {
    const panel = byID("transfer-panel");
    const launcher = byID("open-transfers");
    const headerStatus = byID("header-transfer-status");
    const available = state.transfers.length > 0 || state.transferGroups.size > 0;
    const shouldOpen = available && open;
    if (shouldOpen && focus) state.transferSheetOpener = document.activeElement;
    panel.hidden = !shouldOpen;
    headerStatus.hidden = !available;
    launcher.setAttribute("aria-expanded", String(shouldOpen));
    const modal = shouldOpen && matchMedia("(max-width: 760px)").matches;
    for (const view of byID("authenticated-view").querySelectorAll(".view")) view.inert = modal;
    document.querySelector(".app-header").inert = modal;
    if (focus && shouldOpen) byID("transfer-close").focus();
    if (focus && !shouldOpen && state.transferSheetOpener && state.transferSheetOpener.focus) state.transferSheetOpener.focus();
  }

  async function queueFiles(files, options = {}) {
    const baseDirectory = state.currentDirectory;
    const inputs = Array.from(files).map((value) => value instanceof File ? { file: value, relativePath: value.webkitRelativePath || value.name } : value)
      .filter((value) => value && value.file instanceof File && typeof value.relativePath === "string");
    const groupID = options.groupID || (options.groupName || inputs.length > 1 ? idempotencyKey() : "");
    const queued = [];
    let group = groupID ? state.transferGroups.get(groupID) : null;
    if (groupID && !group) {
      group = {
        id: groupID, name: options.groupName || `${inputs.length} files`, baseDirectory, directories: [], transferIDs: [],
        totalSize: 0, state: "queued", error: "", refreshed: false, cancelled: false, failureAnnounced: false,
        discoveryDone: options.discoveryDone !== false, createdAt: Date.now(),
      };
      state.transferGroups.set(groupID, group);
    }
    const discoveredDirectories = Array.from(new Set(options.directories || [])).filter((relative) => validUploadComponents(relative));
    if (group) group.directories = Array.from(new Set([...(group.directories || []), ...discoveredDirectories]));
    for (const [index, input] of inputs.entries()) {
      const components = validUploadComponents(input.relativePath);
      if (!components) {
        announce(`Skipped an invalid upload path for ${input.file.name}.`, true);
        continue;
      }
      const name = components.pop();
      const relativeDirectory = components.join("/");
      const directory = relativeDirectory ? joinPath(baseDirectory, relativeDirectory) : baseDirectory;
      queued.push({
        id: idempotencyKey(), file: input.file, name, directory, baseDirectory, relativeDirectory,
        relativePath: input.relativePath, groupID, state: "queued", confirmed: 0, error: "", controller: null, uploadID: "",
        size: input.file.size, mediaType: uploadMediaType(input.file, name), lastModified: input.file.lastModified,
        sourceHandle: input.sourceHandle || null, speedBps: 0, lastProgressAt: 0, lastProgressBytes: 0,
        recoveryFailures: 0, retryCount: 0, nextRetryAt: 0, errorCode: "", createdAt: Date.now(),
      });
      const transfer = queued[queued.length - 1];
      state.transfers.push(transfer);
      state.transferByID.set(transfer.id, transfer);
      if (group) {
        group.transferIDs.push(transfer.id);
        group.totalSize += transferFileSize(transfer);
      }
      queueTransferPersistence(transfer);
      if (transfer.sourceHandle) persistTransferSource(transfer, transfer.sourceHandle);
      if ((index + 1) % transferDiscoveryBatchSize === 0) {
        if (group) persistTransferGroup(group);
        rebuildTransferProjection();
        renderTransfers();
        pumpTransfers();
        await wait(0);
      }
    }
    if (!queued.length && !discoveredDirectories.length) return groupID;
    state.transferFilter = "current";
    if (group) {
      group.discoveryDone = options.discoveryDone !== false;
      group.state = group.transferIDs.length ? "queued" : "preparing";
      await persistTransferGroup(group);
      if (!group.transferIDs.length && group.discoveryDone) prepareTransferGroup(group);
    }
    setTransferSheetOpen(true);
    rebuildTransferProjection();
    renderTransfers();
    pumpTransfers();
    return groupID;
  }

  async function queueFolderFiles(files) {
    const groups = new Map();
    const looseFiles = [];
    for (const file of Array.from(files).filter((value) => value instanceof File)) {
      const relativePath = file.webkitRelativePath || file.name;
      const components = validUploadComponents(relativePath);
      if (!components || components.length < 2) {
        looseFiles.push({ file, relativePath });
        continue;
      }
      const root = components[0];
      let group = groups.get(root);
      if (!group) {
        group = { files: [], directories: new Set() };
        groups.set(root, group);
      }
      group.files.push({ file, relativePath });
      for (let length = 1; length < components.length; length += 1) group.directories.add(components.slice(0, length).join("/"));
    }
    for (const [groupName, group] of groups) await queueFiles(group.files, { groupName, directories: [...group.directories] });
    if (looseFiles.length) await queueFiles(looseFiles);
  }

  function readLegacyFile(entry) {
    return new Promise((resolve, reject) => entry.file(resolve, reject));
  }

  function readLegacyDirectory(reader) {
    return new Promise((resolve, reject) => reader.readEntries(resolve, reject));
  }

  async function discoverLegacyEntry(entry, parent, onFile, onDirectory) {
    const relativePath = parent ? `${parent}/${entry.name}` : entry.name;
    if (entry.isFile) {
      await onFile({ file: await readLegacyFile(entry), relativePath });
      return;
    }
    if (!entry.isDirectory) return;
    await onDirectory(relativePath);
    const reader = entry.createReader();
    while (true) {
      const entries = await readLegacyDirectory(reader);
      if (!entries.length) break;
      for (const child of entries) await discoverLegacyEntry(child, relativePath, onFile, onDirectory);
    }
  }

  async function discoverFileSystemHandle(handle, parent, onFile, onDirectory) {
    const relativePath = parent ? `${parent}/${handle.name}` : handle.name;
    if (handle.kind === "file") {
      await onFile({ file: await handle.getFile(), relativePath, sourceHandle: handle });
      return;
    }
    if (handle.kind !== "directory") return;
    await onDirectory(relativePath);
    for await (const child of handle.values()) await discoverFileSystemHandle(child, relativePath, onFile, onDirectory);
  }

  async function discoverTransferGroup(name, discover) {
    const groupID = idempotencyKey();
    const files = [];
    const directories = [];
    const flush = async () => {
      if (!files.length && !directories.length) return;
      const pendingFiles = files.splice(0, files.length);
      const pendingDirectories = directories.splice(0, directories.length);
      await queueFiles(pendingFiles, { groupID, groupName: name, directories: pendingDirectories, discoveryDone: false });
    };
    await discover(async (input) => {
      files.push(input);
      if (files.length >= transferDiscoveryBatchSize) await flush();
    }, async (directory) => {
      directories.push(directory);
      if (directories.length >= transferDiscoveryBatchSize) await flush();
    });
    await flush();
    const group = state.transferGroups.get(groupID);
    if (group) {
      group.discoveryDone = true;
      if (!group.transferIDs.length) await prepareTransferGroup(group);
      else updateTransferGroup(groupID, false);
      await persistTransferGroup(group);
      rebuildTransferProjection();
      renderTransfers();
      pumpTransfers();
    }
  }

  async function queueDroppedItems(dataTransfer) {
    const items = Array.from(dataTransfer.items || []).filter((item) => item.kind === "file");
    if (!items.length) {
      queueFiles(dataTransfer.files || []);
      return;
    }
    const looseFiles = [];
    let usedEntryAPI = false;
    const handlePromises = items.map((item) => typeof item.getAsFileSystemHandle === "function" ? item.getAsFileSystemHandle() : Promise.resolve(null));
    for (const [index, item] of items.entries()) {
      const handle = await handlePromises[index];
      if (handle) {
        usedEntryAPI = true;
        if (handle.kind === "directory") {
          await discoverTransferGroup(handle.name, (onFile, onDirectory) => discoverFileSystemHandle(handle, "", onFile, onDirectory));
        } else {
          await discoverFileSystemHandle(handle, "", async (input) => { looseFiles.push(input); }, async () => {});
        }
        continue;
      }
      const getEntry = typeof item.getAsEntry === "function" ? item.getAsEntry.bind(item) : typeof item.webkitGetAsEntry === "function" ? item.webkitGetAsEntry.bind(item) : null;
      const legacyEntry = getEntry ? getEntry() : null;
      if (legacyEntry) {
        usedEntryAPI = true;
        if (legacyEntry.isDirectory) {
          await discoverTransferGroup(legacyEntry.name, (onFile, onDirectory) => discoverLegacyEntry(legacyEntry, "", onFile, onDirectory));
        } else {
          await discoverLegacyEntry(legacyEntry, "", async (input) => { looseFiles.push(input); }, async () => {});
        }
        continue;
      }
      const file = typeof item.getAsFile === "function" ? item.getAsFile() : null;
      if (file) looseFiles.push({ file, relativePath: file.name });
    }
    if (looseFiles.length) await queueFiles(looseFiles);
    if (!usedEntryAPI && !looseFiles.length) await queueFiles(dataTransfer.files || []);
  }

  async function ensureDirectory(path) {
    let pending = state.directoryPromises.get(path);
    if (pending) return pending;
    pending = (async () => {
      try {
        const existing = await api(`/api/v1/files/stat?path=${encodeURIComponent(path)}`);
        if (existing.kind !== "directory") throw new Error("An upload folder path is already a file.");
        return;
      } catch (error) {
        if (!(error instanceof APIError) || error.status !== 404) throw error;
      }
      try {
        await api("/api/v1/directories", { method: "POST", body: { path, conflict: "fail" } });
      } catch (error) {
        if (!(error instanceof APIError) || error.status !== 409) throw error;
        const existing = await api(`/api/v1/files/stat?path=${encodeURIComponent(path)}`);
        if (existing.kind !== "directory") throw error;
      }
    })();
    state.directoryPromises.set(path, pending);
    try { await pending; }
    finally { if (state.directoryPromises.get(path) === pending) state.directoryPromises.delete(path); }
  }

  async function ensureDirectories(baseDirectory, relativeDirectory) {
    if (!relativeDirectory) return;
    let current = baseDirectory;
    for (const component of relativeDirectory.split("/")) {
      current = joinPath(current, component);
      await ensureDirectory(current);
    }
  }

  async function prepareTransferGroup(group) {
    group.cancelled = false;
    group.state = "preparing";
    group.error = "";
    renderTransfers();
    try {
      const directories = [...group.directories].sort((left, right) => left.split("/").length - right.split("/").length || left.localeCompare(right));
      for (const relativeDirectory of directories) {
        await ensureDirectories(group.baseDirectory, relativeDirectory);
        if (group.cancelled) return;
      }
      if (group.cancelled) return;
      group.state = group.transferIDs.length ? "queued" : "complete";
      if (!group.transferIDs.length && state.currentDirectory === group.baseDirectory) loadDirectory(group.baseDirectory);
      pumpTransfers();
    } catch (error) {
      group.state = "failed";
      group.error = friendlyError(error, "Folder preparation failed.");
      for (const transfer of state.transfers.filter((item) => item.groupID === group.id)) {
        transfer.state = "failed";
        transfer.error = group.error;
      }
      group.failureAnnounced = true;
      announce(`${group.name} failed to upload.`, true);
    }
    renderTransfers();
  }

  function automaticTransferConcurrency() {
    const configuredMaximum = Number(state.config && state.config.maximumTransferConcurrency);
    const maximum = Math.max(1, Math.min(8, Number.isFinite(configuredMaximum) ? configuredMaximum : 8));
    const summary = state.transferSummary || aggregateTransferSummary(state.transfers);
    const pendingCount = summary.counts.queued + summary.counts.preparing + summary.counts.uploading;
    if (pendingCount <= 1) return 1;

    const connection = navigator.connection || navigator.mozConnection || navigator.webkitConnection;
    let networkCeiling = maximum;
    if (connection && connection.saveData) networkCeiling = 1;
    else if (connection && ["slow-2g", "2g"].includes(connection.effectiveType)) networkCeiling = 1;
    else if (connection && connection.effectiveType === "3g") networkCeiling = Math.min(networkCeiling, 2);
    else if (connection && Number.isFinite(connection.downlink)) {
      if (connection.downlink < 1.5) networkCeiling = Math.min(networkCeiling, 1);
      else if (connection.downlink < 5) networkCeiling = Math.min(networkCeiling, 2);
      else if (connection.downlink < 20) networkCeiling = Math.min(networkCeiling, 3);
      else if (connection.downlink < 50) networkCeiling = Math.min(networkCeiling, 4);
    }

    const hardware = Number(navigator.hardwareConcurrency);
    const fallback = Number(state.config && state.config.defaultTransferConcurrency) || 4;
    const hardwareCeiling = Number.isFinite(hardware) && hardware > 0 ? Math.max(2, Math.ceil(hardware / 2)) : fallback;
    const ceiling = Math.max(1, Math.min(maximum, networkCeiling, hardwareCeiling));
    const averageSize = summary.remainingBytes / Math.max(1, summary.active + summary.pending);
    let desired = Math.min(ceiling, 4);
    if (averageSize <= 256 * 1024) desired = Math.min(ceiling, pendingCount > 500 ? ceiling : 6);
    else if (averageSize >= 256 * 1024 * 1024) desired = Math.min(ceiling, 2);
    else if (averageSize >= 64 * 1024 * 1024) desired = Math.min(ceiling, 3);

    const recentFailures = state.transferFailureTimes.filter((value) => Date.now() - value < 10000).length;
    if (summary.active && recentFailures >= Math.ceil(summary.active / 2)) desired = Math.min(desired, Math.max(1, Math.floor(summary.active / 2)));
    return Math.max(1, Math.min(desired, pendingCount));
  }

  function beginTransferMeasurement(transfer) {
    transfer.speedBps = 0;
    transfer.lastProgressBytes = Math.min(transfer.confirmed, transferFileSize(transfer));
    transfer.lastProgressAt = performance.now();
    transfer.recoveryFailures = 0;
  }

  function recordTransferProgress(transfer, confirmed) {
    const priorConfirmed = transfer.confirmed;
    const priorSpeed = transfer.speedBps;
    const next = Math.max(transfer.confirmed, Math.min(confirmed, transferFileSize(transfer)));
    const now = performance.now();
    const elapsed = now - transfer.lastProgressAt;
    const transferred = next - transfer.lastProgressBytes;
    if (transferred > 0 && elapsed > 0) {
      const instantaneous = transferred * 1000 / elapsed;
      transfer.speedBps = transfer.speedBps > 0
        ? transfer.speedBps * (1 - transferProgressSampleWeight) + instantaneous * transferProgressSampleWeight
        : instantaneous;
    }
    transfer.confirmed = next;
    transfer.lastProgressBytes = next;
    transfer.lastProgressAt = now;
    updateAggregateProgress(state.transferSummary, transfer, priorConfirmed, priorSpeed);
    updateAggregateProgress(state.transferGroups.get(transfer.groupID)?.transferSummary, transfer, priorConfirmed, priorSpeed);
    queueTransferPersistence(transfer);
  }

  function scheduleTransferRender() {
    if (state.transferRenderFrame) return;
    state.transferRenderFrame = requestAnimationFrame(() => {
      state.transferRenderFrame = 0;
      renderTransferProgress();
    });
  }

  function scheduleTransferStructureRender() {
    if (state.transferStructureFrame) return;
    state.transferStructureFrame = requestAnimationFrame(() => {
      state.transferStructureFrame = 0;
      renderTransfers();
    });
  }

  function pumpTransfers() {
    if (!navigator.onLine) return;
    const concurrency = automaticTransferConcurrency();
    while (state.activeTransfers < concurrency) {
      const transfer = nextQueuedTransfer();
      if (!transfer) break;
      state.activeTransfers += 1;
      uploadTransfer(transfer).finally(() => {
        state.activeTransfers -= 1;
        updateTransferGroup(transfer.groupID);
        scheduleTransferStructureRender();
        pumpTransfers();
      });
    }
  }

  function nextQueuedTransfer() {
    const count = state.transfers.length;
    for (let checked = 0; checked < count; checked += 1) {
      const index = state.transferQueueCursor % count;
      state.transferQueueCursor = (index + 1) % count;
      const transfer = state.transfers[index];
      if (transfer.state === "queued" && transfer.file instanceof File && (!transfer.groupID || state.transferGroups.get(transfer.groupID)?.state !== "preparing")) return transfer;
    }
    return null;
  }

  function uploadMediaType(file, name) {
    const declared = String(file.type || "").trim().toLocaleLowerCase();
    if (declared && declared !== "application/octet-stream") return declared;
    const extension = name.includes(".") ? name.split(".").pop().toLocaleLowerCase() : "";
    return ({
      arw: "image/x-sony-arw", cr2: "image/x-canon-cr2", cr3: "image/x-canon-cr3", dng: "image/x-adobe-dng",
      nef: "image/x-nikon-nef", orf: "image/x-olympus-orf", pef: "image/x-pentax-pef", raf: "image/x-fuji-raf", rw2: "image/x-panasonic-rw2",
    })[extension] || "application/octet-stream";
  }

  function transitionTransfer(transfer, nextState, error = "", errorCode = "") {
    const priorState = transfer.state;
    const priorSpeed = transfer.speedBps;
    transfer.state = nextState;
    transfer.error = String(error || "").slice(0, 240);
    transfer.errorCode = String(errorCode || "").slice(0, 80);
    if (nextState !== "uploading") transfer.speedBps = 0;
    updateAggregateState(state.transferSummary, transfer, priorState, nextState, priorSpeed);
    updateAggregateState(state.transferGroups.get(transfer.groupID)?.transferSummary, transfer, priorState, nextState, priorSpeed);
    queueTransferPersistence(transfer);
    updateTransferGroup(transfer.groupID, false);
  }

  function transferFailureIsRetryable(error) {
    if (!navigator.onLine) return true;
    if (error instanceof APIError) return error.status === 408 || error.status === 425 || error.status === 429 || error.status >= 500;
    return error instanceof TypeError || (error && error.name === "NetworkError");
  }

  function transferRetryDelay(transfer) {
    const exponential = Math.min(transferRetryMaximumDelay, transferRetryBaseDelay * (2 ** Math.max(0, transfer.retryCount - 1)));
    const jitter = Math.floor(Math.random() * exponential);
    return Math.max(transferRetryBaseDelay, jitter);
  }

  function noteTransferFailure() {
    const now = Date.now();
    state.transferFailureTimes = state.transferFailureTimes.filter((value) => now - value < 10000);
    state.transferFailureTimes.push(now);
    if (state.transferFailureTimes.length >= 3) state.transferCircuitOpenUntil = Math.max(state.transferCircuitOpenUntil, now + 5000);
  }

  function scheduleTransferRetry(transfer, error) {
    if (!navigator.onLine) {
      transitionTransfer(transfer, "paused", "Waiting for a network connection.", "offline");
      return;
    }
    transfer.retryCount = (transfer.retryCount || 0) + 1;
    if (transfer.retryCount > transferRetryLimit) {
      transitionTransfer(transfer, "failed", friendlyError(error, "Upload could not be resumed automatically."), "retry_exhausted");
      return;
    }
    noteTransferFailure();
    const delay = Math.max(transferRetryDelay(transfer), state.transferCircuitOpenUntil - Date.now());
    transfer.nextRetryAt = Date.now() + delay;
    transitionTransfer(transfer, "retry-wait", "Retrying automatically.", "transient");
    window.clearTimeout(state.transferRetryTimers.get(transfer.id));
    state.transferRetryTimers.set(transfer.id, window.setTimeout(() => {
      state.transferRetryTimers.delete(transfer.id);
      if (transfer.state !== "retry-wait") return;
      transfer.nextRetryAt = 0;
      transitionTransfer(transfer, transfer.file instanceof File ? "queued" : "needs-source", transfer.file instanceof File ? "" : "Reconnect the source to continue.");
      rebuildTransferProjection();
      renderTransfers();
      pumpTransfers();
    }, delay));
  }

  function resumePausedTransfers() {
    let resumed = 0;
    for (const transfer of state.transfers) {
      if (!['paused', 'retry-wait'].includes(transfer.state) || !(transfer.file instanceof File)) continue;
      transitionTransfer(transfer, "queued");
      resumed += 1;
    }
    if (resumed) {
      rebuildTransferProjection();
      renderTransfers();
      byID("transfer-live").textContent = `${resumed} transfers resumed.`;
    }
    pumpTransfers();
  }

  async function uploadTransfer(transfer) {
    if (!(transfer.file instanceof File)) {
      transitionTransfer(transfer, "needs-source", "Reconnect the source to continue.", "source_required");
      return;
    }
    transitionTransfer(transfer, "preparing");
    transfer.cancelRequested = false;
    transfer.controller = new AbortController();
    updateTransferGroup(transfer.groupID, false);
    scheduleTransferStructureRender();
    try {
      await ensureDirectories(transfer.baseDirectory, transfer.relativeDirectory);
      const mediaType = transferMediaType(transfer);
      const size = transferFileSize(transfer);
      const capability = await api("/api/v1/uploads", { method: "POST", headers: { "Idempotency-Key": transfer.id }, body: { path: transfer.directory, name: transfer.name, size, mediaType, conflict: "rename", resumable: true }, signal: transfer.controller.signal });
      transfer.uploadID = capability.uploadID;
      transitionTransfer(transfer, "uploading");
      beginTransferMeasurement(transfer);
      updateTransferGroup(transfer.groupID, false);
      scheduleTransferStructureRender();
      await sendFileData(transfer, capability);
      await api(`/api/v1/uploads/${encodeURIComponent(capability.uploadID)}/complete`, { method: "POST", body: { path: joinPath(transfer.directory, transfer.name), size, mediaType }, signal: transfer.controller.signal });
      recordTransferProgress(transfer, size);
      transfer.retryCount = 0;
      transfer.nextRetryAt = 0;
      transitionTransfer(transfer, "complete");
      if (!transfer.groupID) announce(`${transfer.name} uploaded.`);
      if (transfer.directory === state.currentDirectory) await loadDirectory(state.currentDirectory);
    } catch (error) {
      if (error.name === "AbortError" && transfer.suspendedByLogout) {
        return;
      } else if (error.name === "AbortError" && transfer.cancelRequested) {
        transitionTransfer(transfer, "cancelled", "Cancelled", "cancelled");
      } else if (transferFailureIsRetryable(error)) {
        scheduleTransferRetry(transfer, error);
      } else {
        transitionTransfer(transfer, "failed", friendlyError(error, "Upload could not continue."), error instanceof APIError ? error.code : "terminal");
        if (!transfer.groupID) announce(`${transfer.name} failed to upload.`, true);
      }
    } finally {
      transfer.controller = null;
      scheduleTransferStructureRender();
    }
  }

  async function sendFileData(transfer, capability) {
    let offset = transfer.confirmed;
    const headersTemplate = capability.headers || {};
    const framing = capability.framing || "offset-header";
    const size = transferFileSize(transfer);
    const declaredSize = Number.isSafeInteger(capability.declaredSize) ? capability.declaredSize : size;
    const maximum = capability.chunkRules && capability.chunkRules.maximumSize ? capability.chunkRules.maximumSize : size;
    let failures = 0;
    while (offset < size || (size === 0 && offset === 0)) {
      const end = size === 0 ? 0 : Math.min(size, offset + maximum);
      const headers = new Headers(headersTemplate);
      if (framing === "content-range") {
        headers.delete("Upload-Offset");
        headers.set("Content-Range", size === 0 ? `bytes */${declaredSize}` : `bytes ${offset}-${end - 1}/${declaredSize}`);
      } else if (framing === "offset-header") {
        headers.set("Upload-Offset", String(offset));
      } else {
        headers.delete("Upload-Offset");
      }
      let response;
      try {
        response = await fetch(capability.url, { method: capability.method, headers, body: transfer.file.slice(offset, end), signal: transfer.controller.signal });
      } catch (error) {
        if (error.name === "AbortError") throw error;
        response = null;
      }
      if (response && (response.ok || (framing === "content-range" && response.status === 308))) {
        const range = response.headers.get("Range") || "";
        const rangeMatch = /^bytes=0-([0-9]+)$/.exec(range);
        let confirmed = Number.parseInt(response.headers.get("Upload-Offset") || "", 10);
        if (rangeMatch) confirmed = Number.parseInt(rangeMatch[1], 10) + 1;
        if (!Number.isSafeInteger(confirmed) && response.ok) confirmed = end;
        if (!Number.isSafeInteger(confirmed)) {
          const status = await api(`/api/v1/uploads/${encodeURIComponent(capability.uploadID)}`);
          confirmed = status.confirmedOffset;
        }
        offset = Math.max(offset, confirmed);
        recordTransferProgress(transfer, offset);
        failures = 0;
        transfer.recoveryFailures = 0;
        scheduleTransferRender();
        if (size === 0) break;
        continue;
      }
      const status = await api(`/api/v1/uploads/${encodeURIComponent(capability.uploadID)}`);
      if (status.state === "completed") {
        recordTransferProgress(transfer, size);
        return;
      }
      if (status.state === "aborted" || status.state === "expired") {
        const terminal = new Error(status.state === "expired" ? "Upload expired." : "Upload was cancelled.");
        terminal.name = "TerminalUploadError";
        throw terminal;
      }
      offset = status.confirmedOffset;
      recordTransferProgress(transfer, offset);
      failures += 1;
      transfer.recoveryFailures = failures;
      scheduleTransferRender();
      if (failures > 3) throw new TypeError("Upload interrupted after three recovery attempts.");
      await new Promise((resolve) => window.setTimeout(resolve, 250 * (2 ** (failures - 1)) + Math.floor(Math.random() * 100)));
    }
  }

  async function cancelTransfer(transfer) {
    transfer.cancelRequested = true;
    if (transfer.controller) transfer.controller.abort();
    if (transfer.uploadID) {
      try { await api(`/api/v1/uploads/${encodeURIComponent(transfer.uploadID)}`, { method: "DELETE", body: {} }); } catch { /* already expired or complete */ }
    }
    transitionTransfer(transfer, "cancelled", "Cancelled", "cancelled");
    rebuildTransferProjection();
    renderTransfers();
    announce(`${transfer.name} cancelled.`);
  }

  function updateTransferGroup(groupID, notify = true) {
    if (!groupID) return;
    const group = state.transferGroups.get(groupID);
    if (!group || group.state === "preparing") return;
    const summary = group.transferSummary || aggregateTransferSummary(group.transferIDs.map((id) => state.transferByID.get(id)).filter(Boolean));
    group.transferSummary = summary;
    if (group.cancelled) group.state = "cancelled";
    else if (group.discoveryDone !== false && summary.totalCount > 0 && summary.counts.complete === summary.totalCount) group.state = "complete";
    else if (summary.counts.preparing + summary.counts.uploading + summary.counts["retry-wait"] + summary.counts.paused > 0) group.state = "uploading";
    else if (summary.counts.queued + summary.counts["needs-source"] > 0) group.state = "queued";
    else if (summary.counts.failed > 0) group.state = "failed";
    else if (group.discoveryDone === false) group.state = "queued";
    else group.state = "cancelled";
    persistTransferGroup(group);
    if (group.state === "complete" && !group.refreshed) {
      group.refreshed = true;
      if (state.currentDirectory === group.baseDirectory) loadDirectory(group.baseDirectory);
      if (notify) announce(`${group.name} uploaded with ${transfers.length} files.`);
    }
    if (group.state === "failed" && !group.failureAnnounced) {
      group.failureAnnounced = true;
      if (notify) announce(`${group.name} has ${transfers.filter((transfer) => transfer.state === "failed").length} failed uploads.`, true);
    }
  }

  async function cancelTransferGroup(group) {
    group.cancelled = true;
    group.state = "cancelled";
    const cancelled = state.transfers.filter((item) => item.groupID === group.id && ["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(item.state));
    const uploadIDs = [];
    for (const transfer of cancelled) {
      transfer.cancelRequested = true;
      if (transfer.controller) transfer.controller.abort();
      if (transfer.uploadID) uploadIDs.push(transfer.uploadID);
      window.clearTimeout(state.transferRetryTimers.get(transfer.id));
      state.transferRetryTimers.delete(transfer.id);
      transitionTransfer(transfer, "cancelled", "Cancelled", "cancelled");
    }
    await Promise.all(cancelled.map(persistTransferItem));
    await persistTransferGroup(group);
    if (!group.fixture) await Promise.all(uploadIDs.map(async (uploadID) => {
      try { await api(`/api/v1/uploads/${encodeURIComponent(uploadID)}`, { method: "DELETE", body: {} }); } catch { /* already terminal */ }
    }));
    rebuildTransferProjection();
    renderTransfers();
    byID("transfer-live").textContent = `${cancelled.length} transfers cancelled.`;
  }

  function renewTransferIdentity(transfer) {
    const priorID = transfer.id;
    transfer.id = idempotencyKey();
    state.transferByID.delete(priorID);
    state.transferByID.set(transfer.id, transfer);
    const group = state.transferGroups.get(transfer.groupID);
    if (group) {
      group.transferIDs = group.transferIDs.map((id) => id === priorID ? transfer.id : id);
      persistTransferGroup(group);
    }
    deleteTransferLedgerEntries([priorID], []).catch(() => showToast("Old transfer history could not be cleared.", "warning"));
    if (transfer.sourceHandle) persistTransferSource(transfer, transfer.sourceHandle);
  }

  function retryTransferGroup(group) {
    group.cancelled = false;
    group.refreshed = false;
    group.failureAnnounced = false;
    for (const transfer of state.transfers.filter((item) => item.groupID === group.id && ["failed", "cancelled"].includes(item.state))) {
      if (transfer.state === "cancelled" || ["expired", "aborted"].includes(transfer.errorCode)) {
        renewTransferIdentity(transfer);
        transfer.confirmed = 0;
        transfer.uploadID = "";
      }
      transfer.state = "queued";
      transfer.error = "";
      transfer.speedBps = 0;
      transfer.lastProgressAt = 0;
      transfer.lastProgressBytes = transfer.confirmed;
      transfer.recoveryFailures = 0;
      transfer.retryCount = 0;
      transfer.nextRetryAt = 0;
      queueTransferPersistence(transfer);
    }
    if (group.fixture) {
      group.state = "uploading";
      group.error = "";
      const retrying = state.transfers.filter((item) => item.groupID === group.id && item.state === "queued");
      for (const [index, transfer] of retrying.entries()) {
        transfer.state = index < 4 ? "uploading" : "queued";
        transfer.speedBps = index < 4 ? (3 + index) * (1 << 20) : 0;
      }
      rebuildTransferProjection();
      renderTransfers();
      return;
    }
    group.state = "queued";
    persistTransferGroup(group);
    rebuildTransferProjection();
    renderTransfers();
    pumpTransfers();
  }

  function retryTransfer(transfer) {
    if (transfer.state === "cancelled" || ["expired", "aborted"].includes(transfer.errorCode)) {
      renewTransferIdentity(transfer);
      transfer.confirmed = 0;
      transfer.uploadID = "";
    }
    transfer.state = transfer.fixture ? "uploading" : "queued";
    transfer.error = "";
    transfer.speedBps = transfer.fixture ? 4 * (1 << 20) : 0;
    transfer.lastProgressAt = 0;
    transfer.lastProgressBytes = transfer.confirmed;
    transfer.recoveryFailures = 0;
    transfer.retryCount = 0;
    transfer.nextRetryAt = 0;
    queueTransferPersistence(transfer);
    if (!transfer.fixture) pumpTransfers();
    rebuildTransferProjection();
    renderTransfers();
  }

  function retryFailedTransfers() {
    const failed = state.transfers.filter((transfer) => transfer.state === "failed" && transferSourceAvailable(transfer));
    const failedGroupIDs = new Set(failed.map((transfer) => transfer.groupID).filter(Boolean));
    for (const transfer of failed) {
      if (["expired", "aborted"].includes(transfer.errorCode)) {
        renewTransferIdentity(transfer);
        transfer.confirmed = 0;
        transfer.uploadID = "";
      }
      transfer.retryCount = 0;
      transfer.nextRetryAt = 0;
      transitionTransfer(transfer, transfer.fixture ? "uploading" : "queued");
      if (transfer.fixture) transfer.speedBps = 4 * (1 << 20);
    }
    for (const group of state.transferGroups.values()) {
      if (failedGroupIDs.has(group.id)) {
        group.cancelled = false;
        group.failureAnnounced = false;
        group.state = "queued";
        persistTransferGroup(group);
      }
    }
    rebuildTransferProjection();
    renderTransfers();
    pumpTransfers();
    showToast(failed.length ? `${failed.length} failed transfers returned to the queue.` : "No failed transfers can be retried without reconnecting their source.", failed.length ? "success" : "info");
  }

  async function reconnectTransferSources(files) {
    const groupID = state.transferReconnectGroup;
    state.transferReconnectGroup = null;
    const candidates = state.transfers.filter((transfer) => transfer.groupID === groupID && transfer.state === "needs-source");
    const supplied = new Map(Array.from(files).filter((file) => file instanceof File).map((file) => [file.webkitRelativePath || file.name, file]));
    let connected = 0;
    for (const transfer of candidates) {
      const file = supplied.get(transfer.relativePath) || supplied.get(transfer.name);
      if (!file || file.size !== transferFileSize(transfer) || (transfer.lastModified && file.lastModified !== transfer.lastModified)) continue;
      transfer.file = file;
      transitionTransfer(transfer, "queued");
      connected += 1;
    }
    rebuildTransferProjection();
    renderTransfers();
    pumpTransfers();
    showToast(`${connected} of ${candidates.length} transfer sources reconnected.`, connected === candidates.length ? "success" : "warning");
  }

  async function reconnectStoredTransferSources(groupID) {
    const candidates = state.transfers.filter((transfer) => transfer.groupID === groupID && transfer.state === "needs-source");
    let connected = 0;
    for (const transfer of candidates) {
      const handle = transfer.sourceHandle;
      if (!handle || typeof handle.requestPermission !== "function" || typeof handle.getFile !== "function") continue;
      try {
        if (await handle.requestPermission({ mode: "read" }) !== "granted") continue;
        const file = await handle.getFile();
        if (file.size !== transferFileSize(transfer) || (transfer.lastModified && file.lastModified !== transfer.lastModified)) continue;
        transfer.file = file;
        transitionTransfer(transfer, "queued");
        connected += 1;
      } catch {
        // Explicit file selection below remains available when a stored handle
        // cannot be reopened or no longer identifies the original source.
      }
    }
    const remaining = candidates.filter((transfer) => transfer.state === "needs-source");
    if (connected) {
      renderTransfers();
      pumpTransfers();
    }
    if (!remaining.length) {
      showToast(`${connected} transfer sources reconnected.`, "success");
      return;
    }
    state.transferReconnectGroup = groupID;
    byID(groupID ? "folder-input" : "upload-input").click();
  }

  function transferStateLabel(value) {
    return ({ queued: "Queued", preparing: "Preparing", uploading: "Uploading", "retry-wait": "Retrying", paused: "Paused", "needs-source": "Source needed", complete: "Complete", failed: "Failed", cancelled: "Cancelled" })[value] || "Waiting";
  }

  function aggregateTransferSummary(transfers) {
    const counts = { queued: 0, preparing: 0, uploading: 0, "retry-wait": 0, paused: 0, "needs-source": 0, complete: 0, failed: 0, cancelled: 0 };
    let totalBytes = 0;
    let confirmedBytes = 0;
    let remainingBytes = 0;
    let speedBps = 0;
    let retryableFailed = 0;
    for (const transfer of transfers) {
      if (Object.hasOwn(counts, transfer.state)) counts[transfer.state] += 1;
      const size = transferFileSize(transfer);
      totalBytes += size;
      confirmedBytes += Math.min(transfer.confirmed, size);
      if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(transfer.state)) remainingBytes += Math.max(0, size - transfer.confirmed);
      if (transfer.state === "uploading" && Number.isFinite(transfer.speedBps)) speedBps += transfer.speedBps;
      if (transfer.state === "failed" && transferSourceAvailable(transfer)) retryableFailed += 1;
    }
    return finalizeTransferSummary({ counts, totalCount: transfers.length, totalBytes, confirmedBytes, remainingBytes, speedBps, retryableFailed });
  }

  function finalizeTransferSummary(summary) {
    summary.retryableFailed = Number.isSafeInteger(summary.retryableFailed) ? summary.retryableFailed : 0;
    summary.percent = summary.totalBytes > 0
      ? Math.round(summary.confirmedBytes / summary.totalBytes * 100)
      : summary.totalCount > 0 ? Math.round(summary.counts.complete / summary.totalCount * 100) : 0;
    summary.active = summary.counts.preparing + summary.counts.uploading;
    summary.pending = summary.counts.queued + summary.counts["retry-wait"] + summary.counts.paused + summary.counts["needs-source"];
    let eta = "ETA —";
    if (summary.totalCount > 0 && summary.counts.complete === summary.totalCount) eta = "Complete";
    else if (!navigator.onLine && summary.active + summary.pending > 0) eta = "Offline";
    else if (summary.speedBps > 0 && summary.remainingBytes > 0) eta = formatDuration(summary.remainingBytes / summary.speedBps);
    else if (summary.active > 0) eta = "Calculating ETA";
    else if (summary.counts["needs-source"] > 0) eta = "Source needed";
    else if (summary.pending > 0) eta = "Waiting";
    else if (summary.counts.failed > 0) eta = "Needs attention";
    summary.eta = eta;
    return summary;
  }

  function rebuildTransferAggregates() {
    state.transferSummary = aggregateTransferSummary(state.transfers);
    const transfersByGroup = new Map();
    for (const transfer of state.transfers) {
      if (!transfer.groupID) continue;
      if (!transfersByGroup.has(transfer.groupID)) transfersByGroup.set(transfer.groupID, []);
      transfersByGroup.get(transfer.groupID).push(transfer);
    }
    for (const group of state.transferGroups.values()) group.transferSummary = aggregateTransferSummary(transfersByGroup.get(group.id) || []);
  }

  function updateAggregateProgress(summary, transfer, priorConfirmed, priorSpeed) {
    if (!summary) return;
    const size = transferFileSize(transfer);
    const confirmedDelta = Math.min(transfer.confirmed, size) - Math.min(priorConfirmed, size);
    summary.confirmedBytes += confirmedDelta;
    if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(transfer.state)) summary.remainingBytes = Math.max(0, summary.remainingBytes - confirmedDelta);
    if (transfer.state === "uploading") summary.speedBps = Math.max(0, summary.speedBps + transfer.speedBps - priorSpeed);
    finalizeTransferSummary(summary);
  }

  function updateAggregateState(summary, transfer, priorState, nextState, priorSpeed) {
    if (!summary || priorState === nextState) return;
    const size = transferFileSize(transfer);
    const remaining = Math.max(0, size - transfer.confirmed);
    if (Object.hasOwn(summary.counts, priorState)) summary.counts[priorState] = Math.max(0, summary.counts[priorState] - 1);
    if (Object.hasOwn(summary.counts, nextState)) summary.counts[nextState] += 1;
    if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(priorState)) summary.remainingBytes = Math.max(0, summary.remainingBytes - remaining);
    if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(nextState)) summary.remainingBytes += remaining;
    if (priorState === "uploading") summary.speedBps = Math.max(0, summary.speedBps - priorSpeed);
    if (nextState === "uploading") summary.speedBps += transfer.speedBps;
    if (priorState === "failed" && transferSourceAvailable(transfer)) summary.retryableFailed = Math.max(0, summary.retryableFailed - 1);
    if (nextState === "failed" && transferSourceAvailable(transfer)) summary.retryableFailed += 1;
    finalizeTransferSummary(summary);
  }

  function transferMetricText(transfer) {
    const total = transferFileSize(transfer);
    const confirmed = Math.min(transfer.confirmed, total);
    const percent = total > 0 ? Math.round(confirmed / total * 100) : transfer.state === "complete" ? 100 : 0;
    let eta = "Waiting";
    if (transfer.state === "uploading") eta = transfer.speedBps > 0 ? formatDuration((total - confirmed) / transfer.speedBps) : "Calculating ETA";
    else if (transfer.state === "preparing") eta = "Preparing";
    else if (transfer.state === "retry-wait") eta = transfer.nextRetryAt > Date.now() ? `Retry in ${formatDuration((transfer.nextRetryAt - Date.now()) / 1000)}` : "Retrying";
    else if (transfer.state === "paused") eta = "Offline";
    else if (transfer.state === "needs-source") eta = "Source needed";
    else if (transfer.state === "complete") eta = "Complete";
    else if (transfer.state === "failed") eta = "Needs attention";
    else if (transfer.state === "cancelled") eta = "Cancelled";
    return `${percent}% · ${eta} · ${formatRate(transfer.speedBps)}`;
  }

  function transferRow(transfer) {
    const row = document.createElement("div");
    row.className = `transfer-row ${transfer.state}`;
    const label = transfer.groupID ? transfer.relativePath : transfer.name;
    const tail = document.createElement("div");
    tail.className = "transfer-row-tail";
    tail.append(text("span", transferStateLabel(transfer.state), "transfer-row-state"));
    if (transfer.state === "needs-source") tail.append(iconButton("upload", `Reconnect upload source for ${label}`, () => openTransferReconnect(transfer.groupID || ""), "transfer-row-actions", "Reconnect source"));
    if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(transfer.state)) tail.append(iconButton("x", `Cancel upload ${label}`, () => cancelTransfer(transfer), "transfer-row-actions", "Cancel upload"));
    if ((transfer.state === "failed" || (!transfer.groupID && transfer.state === "cancelled")) && transferSourceAvailable(transfer)) {
      tail.append(iconButton("refresh", `Retry upload ${label}`, () => retryTransfer(transfer), "transfer-row-actions", "Retry upload"));
    }
    const metrics = document.createElement("div");
    metrics.className = "transfer-row-metrics";
    metrics.append(
      text("span", transferMetricText(transfer), "transfer-row-metric"),
      text("span", `${formatBytes(transfer.confirmed)} of ${formatBytes(transferFileSize(transfer))}`, "transfer-row-bytes"),
    );
    const progress = document.createElement("progress");
    progress.max = Math.max(1, transferFileSize(transfer));
    progress.value = transferFileSize(transfer) === 0 && transfer.state === "complete" ? 1 : transfer.confirmed;
    progress.setAttribute("aria-label", `Upload progress for ${label}`);
    row.dataset.transferId = transfer.id;
    row.append(text("strong", label, "transfer-row-main"), tail, metrics, progress);
    if (transfer.error) row.append(text("span", transfer.error, "transfer-row-error"));
    return row;
  }

  function groupDisplayState(group, transfers) {
    if (group.error) return "failed";
    if (["preparing", "queued", "uploading"].includes(group.state)) return group.state;
    if (transfers.some((transfer) => transfer.state === "failed")) return "failed";
    return group.state;
  }

  function groupPriorityState(group, transfers) {
    return group.error || transfers.some((transfer) => transfer.state === "failed") ? "failed" : groupDisplayState(group, transfers);
  }

  function transferGroupRow(group, transfers) {
    const summary = group.transferSummary || aggregateTransferSummary(transfers);
    const displayState = groupDisplayState(group, transfers);
    const row = document.createElement("div");
    row.className = `transfer-group-row ${displayState}`;
    const tail = document.createElement("div");
    tail.className = "transfer-row-tail";
    tail.append(text("span", transferStateLabel(displayState), "transfer-row-state"));
    if (transfers.some((transfer) => transfer.state === "needs-source")) tail.append(iconButton("folder-up", `Reconnect upload source for ${group.name}`, () => openTransferReconnect(group.id), "transfer-row-actions", "Reconnect source"));
    if (["preparing", "queued", "uploading"].includes(group.state)) tail.append(iconButton("x", `Cancel folder upload ${group.name}`, () => cancelTransferGroup(group), "transfer-row-actions", "Cancel folder upload"));
    if (["failed", "cancelled"].includes(group.state)) tail.append(iconButton("refresh", `Retry folder upload ${group.name}`, () => retryTransferGroup(group), "transfer-row-actions", "Retry folder upload"));
    const expanded = state.expandedTransferGroups.has(group.id);
    const disclosure = iconButton(expanded ? "chevron-up" : "chevron-down", `${expanded ? "Collapse" : "Expand"} upload group ${group.name}`, () => {
      if (expanded) state.expandedTransferGroups.delete(group.id);
      else state.expandedTransferGroups.add(group.id);
      rebuildTransferProjection();
      renderTransferWindow();
    }, "transfer-row-actions", expanded ? "Collapse group" : "Expand group");
    disclosure.setAttribute("aria-expanded", String(expanded));
    tail.append(disclosure);
    const metrics = document.createElement("div");
    metrics.className = "transfer-row-metrics";
    const metricText = group.state === "preparing" && !transfers.some((transfer) => transfer.state !== "queued")
      ? `${summary.percent}% · Preparing · ${formatRate(summary.speedBps)}`
      : `${summary.percent}% · ${summary.eta} · ${formatRate(summary.speedBps)}`;
    metrics.append(
      text("span", metricText, "transfer-row-metric"),
      text("span", `${summary.counts.complete} of ${transfers.length} files${summary.counts.failed ? ` · ${summary.counts.failed} failed` : ""}`, "transfer-row-bytes"),
    );
    const progress = document.createElement("progress");
    progress.max = group.totalSize > 0 ? group.totalSize : Math.max(1, transfers.length);
    progress.value = group.totalSize > 0 ? summary.confirmedBytes : transfers.length ? summary.counts.complete : group.state === "complete" ? 1 : 0;
    progress.setAttribute("aria-label", `Upload progress for folder ${group.name}`);
    row.dataset.transferGroupId = group.id;
    row.append(text("strong", group.name, "transfer-row-main"), tail, metrics, progress);
    if (group.error) row.append(text("span", group.error, "transfer-row-error"));
    return row;
  }

  function openTransferReconnect(groupID) {
    reconnectStoredTransferSources(groupID).catch(() => {
      state.transferReconnectGroup = groupID;
      byID(groupID ? "folder-input" : "upload-input").click();
    });
  }

  function transferMatchesFilter(item, filter) {
    if (filter === "failed") return item.state === "failed";
    if (filter === "complete") return item.state === "complete";
    if (filter === "current") return !["complete", "cancelled"].includes(item.state);
    return true;
  }

  function setTabSelection(tabList, value) {
    for (const tab of tabList.querySelectorAll("[role=tab][data-tab-value]")) {
      const selected = tab.dataset.tabValue === value;
      tab.setAttribute("aria-selected", String(selected));
      tab.tabIndex = selected ? 0 : -1;
    }
  }

  function bindTabs(tabList, onSelect) {
    const tabs = () => Array.from(tabList.querySelectorAll("[role=tab][data-tab-value]"));
    const select = (tab, focus = false) => {
      setTabSelection(tabList, tab.dataset.tabValue);
      onSelect(tab.dataset.tabValue);
      if (focus) tab.focus();
    };
    tabList.addEventListener("click", (event) => {
      const tab = event.target.closest("[role=tab][data-tab-value]");
      if (tab && tabList.contains(tab)) select(tab);
    });
    tabList.addEventListener("keydown", (event) => {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      const items = tabs();
      const current = Math.max(0, items.indexOf(document.activeElement));
      let next = current;
      if (event.key === "ArrowLeft") next = (current - 1 + items.length) % items.length;
      if (event.key === "ArrowRight") next = (current + 1) % items.length;
      if (event.key === "Home") next = 0;
      if (event.key === "End") next = items.length - 1;
      event.preventDefault();
      select(items[next], true);
    });
  }

  function renderTransferOverview(summary) {
    const progress = byID("transfer-progress");
    progress.max = summary.totalBytes > 0 ? summary.totalBytes : Math.max(1, summary.totalCount);
    progress.value = summary.totalBytes > 0 ? summary.confirmedBytes : summary.counts.complete;
    progress.setAttribute("aria-valuetext", `${summary.percent}% uploaded`);
    byID("transfer-percent").textContent = `${summary.percent}%`;
    byID("transfer-eta").textContent = summary.eta;
    byID("transfer-speed").textContent = formatRate(summary.speedBps);
    byID("transfer-volume").textContent = `${formatBytes(summary.confirmedBytes)} of ${formatBytes(summary.totalBytes)}`;
    const parts = [];
    if (summary.active) parts.push(`${summary.active} active`);
    if (summary.counts.queued) parts.push(`${summary.counts.queued} queued`);
    if (summary.counts["retry-wait"]) parts.push(`${summary.counts["retry-wait"]} retrying`);
    if (summary.counts.paused) parts.push(`${summary.counts.paused} paused`);
    if (summary.counts["needs-source"]) parts.push(`${summary.counts["needs-source"]} need source`);
    if (summary.counts.failed) parts.push(`${summary.counts.failed} failed`);
    if (summary.counts.complete) parts.push(`${summary.counts.complete} complete`);
    if (summary.counts.cancelled) parts.push(`${summary.counts.cancelled} cancelled`);
    byID("transfer-counts").textContent = parts.join(" · ") || "0 files";
    const filterCounts = {
      current: summary.active + summary.pending + summary.counts.failed,
      failed: summary.counts.failed,
      complete: summary.counts.complete,
      all: summary.totalCount,
    };
    const transferTabs = byID("transfer-filter");
    setTabSelection(transferTabs, state.transferFilter);
    for (const [value, count] of Object.entries(filterCounts)) {
      transferTabs.querySelector(`[data-transfer-count="${value}"]`).textContent = `(${count})`;
    }
    byID("header-transfer-percent").textContent = `${summary.percent}%`;
    byID("header-transfer-speed").textContent = formatRate(summary.speedBps);
    byID("header-transfer-eta").textContent = summary.eta.replace(" remaining", "").replace("Calculating ETA", "ETA…").replace("Needs attention", "Attention");
    byID("header-transfer-count").textContent = `${summary.active.toLocaleString()}/${summary.totalCount.toLocaleString()}`;
    setIconControl(byID("open-transfers"), "transfer", `View transfers, ${summary.percent}% complete, ${formatRate(summary.speedBps)}, ${summary.eta}, ${summary.active} active of ${summary.totalCount} files`);
    byID("clear-transfers").disabled = summary.counts.complete + summary.counts.cancelled === 0;
    byID("retry-failed-transfers").disabled = summary.retryableFailed === 0;
  }

  function orderedTransfers(items) {
    const rank = { failed: 0, "needs-source": 1, uploading: 2, preparing: 3, "retry-wait": 4, paused: 5, queued: 6, cancelled: 7, complete: 8 };
    return items.map((item, index) => ({ item, index })).sort((left, right) => {
      const stateOrder = (rank[left.item.state] ?? 9) - (rank[right.item.state] ?? 9);
      if (stateOrder) return stateOrder;
      if (["complete", "cancelled"].includes(left.item.state)) return right.index - left.index;
      return left.index - right.index;
    }).map(({ item }) => item);
  }

  function transferMatchesSearch(transfer) {
    const query = state.transferSearch.trim().toLocaleLowerCase();
    return !query || `${transfer.relativePath || transfer.name} ${transfer.directory}`.toLocaleLowerCase().includes(query);
  }

  function rebuildTransferProjection() {
    const filter = state.transferFilter;
    const projection = [];
    const standaloneTransfers = [];
    const transfersByGroup = new Map();
    for (const transfer of state.transfers) {
      if (!transfer.groupID) {
        standaloneTransfers.push(transfer);
        continue;
      }
      if (!transfersByGroup.has(transfer.groupID)) transfersByGroup.set(transfer.groupID, []);
      transfersByGroup.get(transfer.groupID).push(transfer);
    }
    for (const transfer of orderedTransfers(standaloneTransfers.filter((item) => transferMatchesFilter(item, filter) && transferMatchesSearch(item)))) {
      projection.push({ kind: "transfer", transfer, nested: false });
    }
    const groupItems = [...state.transferGroups.values()].filter((group) => {
      const transfers = transfersByGroup.get(group.id) || [];
      return transfers.some((item) => transferMatchesFilter(item, filter) && transferMatchesSearch(item))
        || (!transfers.length && (filter === "all" || transferMatchesFilter(group, filter)) && (!state.transferSearch || group.name.toLocaleLowerCase().includes(state.transferSearch.toLocaleLowerCase())));
    });
    const groups = orderedTransfers(groupItems.map((group) => ({ ...group, state: groupPriorityState(group, transfersByGroup.get(group.id) || []) })));
    for (const groupView of groups) {
      const group = state.transferGroups.get(groupView.id);
      const transfers = transfersByGroup.get(group.id) || [];
      projection.push({ kind: "group", group, transfers });
      const filteredFiles = orderedTransfers(transfers.filter((transfer) => transferMatchesFilter(transfer, filter) && transferMatchesSearch(transfer)));
      const showAll = state.expandedTransferGroups.has(group.id) || filter === "failed" || Boolean(state.transferSearch);
      for (const transfer of filteredFiles) {
        if (showAll || ["failed", "needs-source", "uploading", "preparing", "retry-wait", "paused"].includes(transfer.state)) {
          projection.push({ kind: "transfer", transfer, nested: true });
        }
      }
    }
    state.transferProjection = projection;
    state.transferVirtualStart = Math.min(state.transferVirtualStart, Math.max(0, projection.length - transferVirtualWindowSize));
  }

  function renderTransferWindow() {
    const list = byID("transfer-list");
    list.replaceChildren();
    list.dataset.itemCount = String(state.transferProjection.length);
    list.dataset.virtualStart = String(state.transferVirtualStart);
    if (!state.transferProjection.length) {
      const empty = text("li", state.transferSearch ? "No matching transfers" : "No transfers", "transfer-empty");
      list.append(empty);
      list.dataset.renderedCount = "0";
      return;
    }
    const start = state.transferVirtualStart;
    const end = Math.min(state.transferProjection.length, start + transferVirtualWindowSize);
    list.dataset.renderedCount = String(end - start);
    for (const [offset, view] of state.transferProjection.slice(start, end).entries()) {
      const item = document.createElement("li");
      item.className = view.nested ? "transfer-item nested" : "transfer-item";
      item.setAttribute("aria-posinset", String(start + offset + 1));
      item.setAttribute("aria-setsize", String(state.transferProjection.length));
      item.append(view.kind === "group" ? transferGroupRow(view.group, view.transfers) : transferRow(view.transfer));
      list.append(item);
    }
    list.setAttribute("aria-label", `Transfer items ${start + 1} to ${end} of ${state.transferProjection.length}`);
  }

  function shiftTransferWindow(direction) {
    const list = byID("transfer-list");
    const priorStart = state.transferVirtualStart;
    const maximumStart = Math.max(0, state.transferProjection.length - transferVirtualWindowSize);
    state.transferVirtualStart = Math.max(0, Math.min(maximumStart, priorStart + direction * transferVirtualWindowStep));
    const moved = state.transferVirtualStart - priorStart;
    if (!moved) return;
    const priorScroll = list.scrollTop;
    renderTransferWindow();
    list.scrollTop = Math.max(1, priorScroll - moved * transferVirtualRowHeight);
  }

  function scheduleTransferWindow(direction) {
    if (state.transferVirtualFrame) return;
    state.transferVirtualFrame = requestAnimationFrame(() => {
      state.transferVirtualFrame = 0;
      shiftTransferWindow(direction);
    });
  }

  function updateRenderedTransferProgress() {
    for (const row of byID("transfer-list").querySelectorAll("[data-transfer-id], [data-transfer-group-id]")) {
      const progress = row.querySelector("progress");
      const metric = row.querySelector(".transfer-row-metric");
      const bytes = row.querySelector(".transfer-row-bytes");
      if (row.dataset.transferId) {
        const transfer = state.transferByID.get(row.dataset.transferId);
        if (!transfer) continue;
        const size = transferFileSize(transfer);
        metric.textContent = transferMetricText(transfer);
        bytes.textContent = `${formatBytes(transfer.confirmed)} of ${formatBytes(size)}`;
        progress.max = Math.max(1, size);
        progress.value = size === 0 && transfer.state === "complete" ? 1 : transfer.confirmed;
        continue;
      }
      const group = state.transferGroups.get(row.dataset.transferGroupId);
      const summary = group && group.transferSummary;
      if (!group || !summary) continue;
      metric.textContent = group.state === "preparing"
        ? `${summary.percent}% · Preparing · ${formatRate(summary.speedBps)}`
        : `${summary.percent}% · ${summary.eta} · ${formatRate(summary.speedBps)}`;
      bytes.textContent = `${summary.counts.complete} of ${summary.totalCount} files${summary.counts.failed ? ` · ${summary.counts.failed} failed` : ""}`;
      progress.max = group.totalSize > 0 ? group.totalSize : Math.max(1, summary.totalCount);
      progress.value = group.totalSize > 0 ? summary.confirmedBytes : summary.totalCount ? summary.counts.complete : group.state === "complete" ? 1 : 0;
    }
  }

  function renderTransferProgress() {
    renderTransferOverview(state.transferSummary || aggregateTransferSummary(state.transfers));
    updateRenderedTransferProgress();
  }

  function renderTransfers() {
    rebuildTransferProjection();
    rebuildTransferAggregates();
    renderTransferOverview(state.transferSummary);
    renderTransferWindow();

  }

  function seedTransferPreviewFixture() {
    if (!state.config || !state.config.localFixture || new URLSearchParams(location.search).get("fixture") !== "transfers") return;
    if (state.transfers.length > 0 || state.transferGroups.size > 0) return;

    const groupID = "local-transfer-preview";
    const baseDirectory = "/Photography";
    let totalSize = 0;
    for (let index = 0; index < 2000; index += 1) {
      const size = (1 + (index % 24)) * (1 << 20);
      let transferState = "queued";
      if (index < 4) transferState = "uploading";
      else if (index < 12) transferState = "failed";
      else if (index < 132) transferState = "complete";
      else if (index < 140) transferState = "cancelled";
      const confirmed = transferState === "complete" ? size
        : transferState === "uploading" ? Math.floor(size * (0.32 + index * 0.13))
        : transferState === "failed" ? Math.floor(size * 0.18) : 0;
      const name = `Campaign asset ${String(index + 1).padStart(4, "0")}.jpg`;
      totalSize += size;
      state.transfers.push({
        id: `local-transfer-${index + 1}`,
        file: { name, size, type: "image/jpeg" },
        name,
        directory: baseDirectory,
        baseDirectory,
        relativeDirectory: "",
        relativePath: name,
        groupID,
        state: transferState,
        confirmed,
        size,
        mediaType: "image/jpeg",
        lastModified: 0,
        error: transferState === "failed" ? "Upload interrupted. Retry when ready." : transferState === "cancelled" ? "Cancelled" : "",
        controller: null,
        uploadID: "",
        speedBps: transferState === "uploading" ? (3 + index) * (1 << 20) : 0,
        lastProgressAt: 0,
        lastProgressBytes: confirmed,
        recoveryFailures: transferState === "failed" ? 1 : 0,
        retryCount: transferState === "failed" ? 1 : 0,
        nextRetryAt: 0,
        errorCode: transferState === "failed" ? "fixture_failure" : "",
        fixture: true,
      });
    }
    state.transferByID = new Map(state.transfers.map((transfer) => [transfer.id, transfer]));
    state.transferGroups.set(groupID, {
      id: groupID,
      name: "Campaign archive",
      baseDirectory,
      directories: [],
      transferIDs: state.transfers.map((transfer) => transfer.id),
      totalSize,
      state: "uploading",
      error: "",
      refreshed: false,
      cancelled: false,
      failureAnnounced: false,
      discoveryDone: true,
      fixture: true,
    });
    state.transferFilter = "current";
    renderTransfers();
    setTransferSheetOpen(true);

    state.transferFixtureTimer = window.setInterval(() => {
      let changed = false;
      for (const transfer of state.transfers.filter((item) => item.fixture && item.state === "uploading")) {
        const ceiling = Math.floor(transferFileSize(transfer) * 0.94);
        const next = Math.min(ceiling, transfer.confirmed + Math.max(1, transfer.speedBps));
        if (next !== transfer.confirmed) {
          recordTransferProgress(transfer, next);
          changed = true;
        }
      }
      if (changed) scheduleTransferRender();
    }, 1000);
  }

  async function download(entry, publicToken = "") {
    try {
      const endpoint = publicToken ? `/api/v1/public/shares/${encodeURIComponent(publicToken)}/downloads` : "/api/v1/downloads";
      const created = await api(endpoint, { method: "POST", body: { path: publicToken ? entry.path : entry.path, version: entry.version } });
      const link = document.createElement("a");
      link.href = created.capability.url;
      link.download = entry.name;
      link.rel = "noreferrer";
      document.body.append(link);
      link.click();
      link.remove();
      announce(`Download started for ${entry.name}.`);
    } catch (error) { showActionErrorToast("Download", error, "Download could not start."); }
  }

  async function preview(entry, publicToken = "") {
    if (!publicToken) {
      openMediaViewer(entry);
      return;
    }
    await openSafeOriginalPreview(entry, publicToken);
  }

  function populatePreviewMetadata(entry) {
    byID("preview-title").textContent = entry.name;
    const metadata = byID("preview-metadata");
    metadata.replaceChildren();
    for (const [label, value] of [["Location", entry.path], ["Size", formatBytes(entry.size)], ["Media type", entry.mediaType || "Unknown"], ["Changed", formatDate(entry.modifiedAt)]]) {
      const group = document.createElement("div");
      group.append(text("dt", label), text("dd", value));
      metadata.append(group);
    }
  }

  function syncViewerNavigation(standalone = false) {
    byID("preview-previous").hidden = false;
    byID("preview-next").hidden = false;
    byID("preview-previous").disabled = standalone || state.viewerIndex <= 0;
    byID("preview-next").disabled = standalone || state.viewerIndex < 0 || state.viewerIndex >= state.viewerEntries.length - 1;
  }

  async function openSafeOriginalPreview(entry, publicToken = "") {
    if (entry.kind !== "file") return;
    clearToast();
    const dialog = byID("preview-dialog");
    state.viewerEntry = entry;
    if (state.viewerController) state.viewerController.abort();
    state.viewerController = new AbortController();
    const signal = state.viewerController.signal;
    releaseViewerObjectURL();
    populatePreviewMetadata(entry);
    syncViewerNavigation(Boolean(publicToken) || state.viewerIndex < 0);
    syncPreviewGenerationActions(false, false);
    byID("preview-original").hidden = false;
    byID("preview-original").disabled = true;
    byID("preview-download").hidden = false;
    byID("preview-download").onclick = () => download(entry, publicToken);
    const content = byID("preview-content");
    showViewerFallback(entry);
    if (!dialog.open) { state.viewerOpener = document.activeElement; showTopLayerDialog(dialog); }
    try {
      const endpoint = publicToken ? `/api/v1/public/shares/${encodeURIComponent(publicToken)}/downloads` : "/api/v1/downloads";
      const created = await api(endpoint, { method: "POST", body: { path: entry.path, version: entry.version, preview: true }, signal });
      content.replaceChildren();
      if (created.mode === "text") {
        const response = await fetch(created.capability.url, { headers: created.capability.headers || {}, signal });
        if (!response.ok) throw new Error("Preview data was unavailable.");
        content.append(text("pre", await response.text()));
      } else if (created.mode === "image") {
        if (signal.aborted) return;
        const image = document.createElement("img");
        image.src = created.capability.url;
        image.alt = `Preview of ${entry.name}`;
        content.append(image);
      } else if (created.mode === "pdf") {
        if (signal.aborted) return;
        const frame = document.createElement("iframe");
        frame.src = created.capability.url;
        frame.title = `Preview of ${entry.name}`;
        content.append(frame);
      } else {
        showViewerFallback(entry);
        showToast("Browser preview unavailable. Download the original.", "info");
      }
    } catch (error) {
      if (error.name === "AbortError") return;
      showViewerFallback(entry);
      showActionErrorToast("Show original", error, "Preview is unavailable.");
    }
  }

  function openMediaViewer(entry) {
    if (entry.kind !== "file") return;
    state.viewerEntries = state.filteredEntries.filter((item) => item.kind === "file");
    state.viewerIndex = state.viewerEntries.findIndex((item) => item.path === entry.path);
    if (state.viewerIndex < 0) { state.viewerEntries = [entry]; state.viewerIndex = 0; }
    state.viewerOpener = document.activeElement;
    const dialog = byID("preview-dialog");
    if (!dialog.open) showTopLayerDialog(dialog);
    showViewerEntry(state.viewerEntries[state.viewerIndex]);
  }

  function viewerPreviewKey(entry, variant) {
    return `${entry.path}\u0000${entry.version}\u0000${variant}`;
  }

  function deleteCachedViewerPreview(key) {
    const cached = state.viewerPreviewCache.get(key);
    if (!cached) return;
    state.viewerPreviewCache.delete(key);
    state.viewerPreviewCacheBytes = Math.max(0, state.viewerPreviewCacheBytes - cached.blob.size);
  }

  function cachedViewerPreview(entry, variant) {
    const key = viewerPreviewKey(entry, variant);
    const cached = state.viewerPreviewCache.get(key);
    if (!cached) return null;
    if (!Number.isFinite(cached.expiresAt) || Date.now() >= cached.expiresAt) {
      deleteCachedViewerPreview(key);
      return null;
    }
    state.viewerPreviewCache.delete(key);
    state.viewerPreviewCache.set(key, cached);
    return cached;
  }

  function cacheViewerPreview(entry, variant, result, blob) {
    const expiresAt = Date.parse(result.capability && result.capability.expiresAt || "");
    if (!Number.isFinite(expiresAt) || expiresAt <= Date.now() || blob.size > maximumViewerPreviewCacheBytes) return;
    const key = viewerPreviewKey(entry, variant);
    deleteCachedViewerPreview(key);
    state.viewerPreviewCache.set(key, { blob, artifact: result.artifact, expiresAt });
    state.viewerPreviewCacheBytes += blob.size;
    while (state.viewerPreviewCache.size > maximumViewerPreviewCacheEntries || state.viewerPreviewCacheBytes > maximumViewerPreviewCacheBytes) {
      deleteCachedViewerPreview(state.viewerPreviewCache.keys().next().value);
    }
  }

  function clearViewerPreviewCache() {
    state.viewerPreviewCache.clear();
    state.viewerPreviewCacheBytes = 0;
  }

  function syncPreviewGenerationActions(canGenerate, generated) {
    byID("preview-generation-action").hidden = !canGenerate;
    byID("preview-generate").hidden = !canGenerate || generated;
    byID("preview-regenerate").hidden = !canGenerate || !generated;
  }

  async function showViewerEntry(entry) {
    clearToast();
    state.viewerEntry = entry;
    if (state.viewerController) state.viewerController.abort();
    state.viewerController = new AbortController();
    releaseViewerObjectURL();
    populatePreviewMetadata(entry);
    syncViewerNavigation();
    const canGenerate = Boolean(state.config && state.config.previewConfigured && mediaCategory(entry) === "image");
    const knownPreview = state.previewStates.get(entry.path);
    syncPreviewGenerationActions(canGenerate, knownPreview?.state === "ready");
    byID("preview-original").hidden = false;
    byID("preview-original").disabled = false;
    byID("preview-download").hidden = false;
    byID("preview-download").onclick = () => download(entry);
    const variant = previewVariant(Math.max(window.innerWidth, window.innerHeight));
    const cached = cachedViewerPreview(entry, variant);
    if (cached) {
      syncPreviewGenerationActions(canGenerate, true);
      try {
        await displayViewerBlob(entry, cached.artifact, cached.blob, state.viewerController.signal);
        return;
      } catch (error) {
        if (error.name === "AbortError") return;
        deleteCachedViewerPreview(viewerPreviewKey(entry, variant));
        releaseViewerObjectURL();
      }
    }
    showViewerFallback(entry);
    if (!state.config || !state.config.previewConfigured) {
      showToast("Generated previews are not configured.", "info");
      return;
    }
    try {
      const response = await api("/api/v1/previews/resolve", { method: "POST", body: { items: [{ path: entry.path, version: entry.version, variant }] }, signal: state.viewerController.signal });
      await displayViewerResult(entry, response.items[0], state.viewerController.signal, variant);
    } catch (error) {
      if (error.name !== "AbortError") {
        showActionErrorToast("Preview", error, "Generated preview is unavailable.");
        showViewerFallback(entry);
      }
    }
  }

  function showViewerFallback(entry) {
    const fallback = fileTypeIcon(entry, "viewer-fallback-icon");
    fallback.setAttribute("role", "img");
    fallback.setAttribute("aria-label", `${mediaCategory(entry)} file icon for ${entry.name}`);
    fallback.removeAttribute("aria-hidden");
    byID("preview-content").replaceChildren(fallback);
  }

  function showPreviewIssue(result) {
    if (result.state === "unsupported") return;
    const issues = {
      disabled: ["Generated previews are not configured.", "info"],
      ineligible: ["Automatic preview generation is disabled for this file.", "info"],
      missing: ["No preview has been generated.", "info"],
      generating: ["Preview generation continues.", "info"],
      unavailable: ["Preview unavailable. Original file unaffected.", "warning"],
      failed: ["Preview generation failed.", "error"],
    };
    const issue = issues[result.state] || ["Preview unavailable.", "error"];
    showToast(issue[0], issue[1]);
  }

  async function displayViewerResult(entry, result, signal, variant) {
    state.previewStates.set(entry.path, result);
    const canGenerate = Boolean(state.config && state.config.previewConfigured && mediaCategory(entry) === "image");
    syncPreviewGenerationActions(canGenerate, result.state === "ready");
    if (result.state !== "ready" || !result.capability) {
      showPreviewIssue(result);
      showViewerFallback(entry);
      return;
    }
    const response = await fetch(result.capability.url, { method: result.capability.method || "GET", headers: result.capability.headers || {}, signal, credentials: "omit" });
    const blob = await validatedPreviewBlob(response, result.artifact);
    await displayViewerBlob(entry, result.artifact, blob, signal);
    cacheViewerPreview(entry, variant, result, blob);
  }

  async function displayViewerBlob(entry, artifact, blob, signal) {
    releaseViewerObjectURL();
    state.viewerObjectURL = URL.createObjectURL(blob);
    const image = document.createElement("img");
    image.src = state.viewerObjectURL;
    image.alt = `Preview of ${entry.name}`;
    image.width = artifact.width;
    image.height = artifact.height;
    image.decoding = "async";
    try {
      await image.decode();
    } catch (error) {
      if (signal.aborted) throw new DOMException("Aborted", "AbortError");
      throw error;
    }
    if (signal.aborted) throw new DOMException("Aborted", "AbortError");
    clearToast();
    byID("preview-content").replaceChildren(image);
  }

  async function generateViewerPreview(regenerate) {
    const entry = state.viewerEntry;
    if (!entry) return;
    const action = regenerate ? "regenerate" : "generate";
    const control = byID(regenerate ? "preview-regenerate" : "preview-generate");
    control.disabled = true;
    try {
      const variant = previewVariant(Math.max(window.innerWidth, window.innerHeight));
      const requestIdentity = `${entry.path}\u0000${entry.version}\u0000${variant}\u0000${action}`;
      let pending = state.previewGenerationRequests.get(requestIdentity);
      if (!pending) {
        if (state.previewGenerationRequests.size >= 64) state.previewGenerationRequests.delete(state.previewGenerationRequests.keys().next().value);
        pending = { idempotencyKey: idempotencyKey(), body: { path: entry.path, version: entry.version, variant, action } };
        state.previewGenerationRequests.set(requestIdentity, pending);
      }
      const postGeneration = () => api("/api/v1/previews/generations", {
        method: "POST", headers: { "Idempotency-Key": pending.idempotencyKey }, body: pending.body, signal: state.viewerController.signal,
      });
      let operation = await postGeneration();
      if (operation.state === "running") showToast("Preview generation continues.", "info");
      operation = await waitForPreviewOperation(operation, postGeneration, state.viewerController.signal);
      if (operation.state === "succeeded" && operation.result) {
        state.previewGenerationRequests.delete(requestIdentity);
        await displayViewerResult(entry, operation.result, state.viewerController.signal, variant);
      } else if (operation.state === "failed") {
        state.previewGenerationRequests.delete(requestIdentity);
        showActionErrorToast(regenerate ? "Regenerate preview" : "Generate preview", null, "Preview generation did not complete. Try again.");
        showViewerFallback(entry);
      } else {
        showToast("Preview generation is still running.", "warning", 10000);
        showViewerFallback(entry);
      }
    } catch (error) {
      if (error.name !== "AbortError") {
        showActionErrorToast(regenerate ? "Regenerate preview" : "Generate preview", error, "Preview generation did not complete. Try again.");
        showViewerFallback(entry);
      }
    } finally {
      control.disabled = false;
    }
  }

  async function waitForPreviewOperation(initial, retryMutation, signal) {
    let operation = initial;
    for (let attempt = 0; attempt < maximumPreviewPolls && operation.state === "running"; attempt += 1) {
      const delay = previewRetryDelays[Math.min(attempt, previewRetryDelays.length - 1)];
      await wait(delay, signal);
      if (attempt >= 12 && attempt % 2 === 0) operation = await retryMutation();
      else operation = await api(`/api/v1/previews/operations/${encodeURIComponent(operation.id)}`, { signal });
    }
    return operation;
  }

  function navigateViewer(offset) {
    const next = state.viewerIndex + offset;
    if (next < 0 || next >= state.viewerEntries.length) return;
    state.viewerIndex = next;
    showViewerEntry(state.viewerEntries[next]);
  }

  function releaseViewerObjectURL() {
    if (state.viewerObjectURL) URL.revokeObjectURL(state.viewerObjectURL);
    state.viewerObjectURL = "";
  }

  async function copyMove(move, entries = [...state.selected.values()], clearSelection = true) {
    const result = await ask({ title: `${move ? "Move" : "Copy"} ${entries.length} item${entries.length === 1 ? "" : "s"}`, description: "Enter a destination folder path. Names are kept unchanged.", confirm: move ? "Move" : "Copy", fields: [{ id: "destination", label: "Destination folder", value: state.currentDirectory, required: true }], conflictField: true });
    if (!result) return;
    const items = entries.map((entry) => ({ source: entry.path, destination: joinPath(result.destination.replace(/\/$/, "") || "/", entry.name), conflict: result.conflict }));
    try {
      const endpoint = move ? "/api/v1/files/move" : "/api/v1/files/copy";
      const operation = await api(endpoint, { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: items.length === 1 ? items[0] : { items } });
      announce(`${move ? "Move" : "Copy"} started for ${items.length} item${items.length === 1 ? "" : "s"}.`);
      if (clearSelection) state.selected.clear();
      else if (move) for (const entry of entries) state.selected.delete(entry.path);
      updateSelection();
      await watchOperation(operation.operationID || operation.id);
      await loadDirectory(state.currentDirectory);
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function trashSelected() {
    return trashEntries([...state.selected.values()], true);
  }

  async function trashEntries(entries, clearSelection) {
    const paths = entries.map((entry) => entry.path);
    if (!paths.length) return;
    try {
      const result = await api("/api/v1/files/trash", { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: { paths } });
      if (clearSelection) state.selected.clear();
      else for (const entry of entries) state.selected.delete(entry.path);
      updateSelection();
      if (result.operationID) await watchOperation(result.operationID);
      await loadDirectory(state.currentDirectory);
      showTrashUndo(result.items || []);
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function watchOperation(operationID) {
    if (!operationID) return;
    for (let attempt = 0; attempt < 50; attempt += 1) {
      const operation = await api(`/api/v1/operations/${encodeURIComponent(operationID)}`);
      announce(`Operation ${operation.state}.`);
      if (operation.state === "succeeded") return;
      if (operation.state === "failed") throw new Error(operation.error || "Operation failed.");
      await new Promise((resolve) => window.setTimeout(resolve, 100 + attempt * 20));
    }
    throw new Error("Operation is still running. Refresh to check again.");
  }

  async function loadTrash(append = false) {
    if (append && state.trashLoading) return;
    const request = state.trashRequest + 1;
    state.trashRequest = request;
    state.trashLoading = true;
    if (!append) {
      cleanupGridMedia(new Set());
      state.previewStates.clear();
      state.entries = [];
      state.trashCursor = "";
      byID("list-presentation").scrollTop = 0;
      byID("media-grid").scrollTop = 0;
      showState("drive-state", "");
      renderFileLoadingItems();
    }
    try {
      const cursor = append && state.trashCursor ? `&cursor=${encodeURIComponent(state.trashCursor)}` : "";
      const page = await api(`/api/v1/trash?limit=100${cursor}`);
      if (request !== state.trashRequest || state.browserAccess !== "trash") return;
      const entries = (page.items || []).map(trashBrowserEntry);
      state.entries = append ? state.entries.concat(entries) : entries;
      state.trashCursor = page.nextCursor || "";
      renderFiles();
      byID("empty-trash").disabled = state.entries.length === 0;
      announce(entries.length ? `${entries.length} trashed item${entries.length === 1 ? "" : "s"} loaded.` : "Trash is empty.");
    } catch (error) {
      if (request !== state.trashRequest || state.browserAccess !== "trash") return;
      if (append) announce(friendlyError(error, "More trashed items could not be loaded."), true);
      else {
        state.entries = [];
        byID("file-rows").replaceChildren();
        byID("media-grid-content").replaceChildren();
        finishListLoading("file-rows");
        finishListLoading("media-grid-content");
        showState("drive-state", friendlyError(error, "Trash could not be loaded."), "error");
        byID("empty-trash").disabled = true;
      }
    } finally {
      if (request === state.trashRequest) state.trashLoading = false;
    }
  }

  function trashBrowserEntry(item) {
    return {
      path: item.originalPath,
      name: item.originalPath,
      kind: item.kind,
      size: item.size || 0,
      mediaType: item.mediaType || "",
      modifiedAt: item.trashedAt,
      version: item.originalVersion,
      trash: item,
    };
  }

  async function restoreTrash(item) {
    const result = await ask({ title: `Restore ${item.originalPath}?`, description: "The item returns to its original location.", confirm: "Restore", conflictField: true });
    if (!result) return;
    try {
      const operation = await api(`/api/v1/trash/${encodeURIComponent(item.trashID)}/restore`, { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: { conflict: result.conflict } });
      await watchOperation(operation.id || operation.operationID); announce(`${item.originalPath} restored.`); await loadTrash();
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function deleteTrash(item) {
    const confirmed = await ask({ title: `Permanently delete ${item.originalPath}?`, description: "This cannot be undone. The file data will be permanently removed.", confirm: "Delete Permanently", danger: true });
    if (!confirmed) return;
    try {
      const operation = await api(`/api/v1/trash/${encodeURIComponent(item.trashID)}`, { method: "DELETE", headers: { "Idempotency-Key": idempotencyKey() }, body: {} });
      await watchOperation(operation.id || operation.operationID); announce(`${item.originalPath} permanently deleted.`); await loadTrash();
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function emptyTrash() {
    const confirmed = await ask({ title: "Permanently delete every trashed item?", description: "All items in Trash will be removed. This cannot be undone.", confirm: "Empty Trash Permanently", danger: true });
    if (!confirmed) return;
    try {
      const result = await api("/api/v1/trash/empty", { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: { confirm: true } });
      if (result.operationID) await watchOperation(result.operationID);
      announce("Trash emptied."); await loadTrash();
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function createShare(entry) {
    const result = await ask({ title: `Share ${entry.name}?`, description: "Anyone with the generated link can view this exact version until it is revoked, expires, moves, or enters trash.", confirm: "Create link", fields: [{ id: "expires", label: "Expiry (optional)", type: "datetime-local" }] });
    if (!result) return;
    try {
      const body = { path: entry.path };
      if (result.expires) body.expiresAt = new Date(result.expires).toISOString();
      const created = await api("/api/v1/shares", { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body });
      await revealLink("Share link", created.link);
      announce(`Share created for ${entry.name}.`);
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function loadSettings() {
    byID("profile-name").value = state.user.displayName;
    byID("passkey-table-scroll").scrollTop = 0;
    byID("share-table-scroll").scrollTop = 0;
    renderTableLoadingRows("passkey-list", "passkey", 4, 3);
    renderTableLoadingRows("share-list", "share", 6, 3);
    try {
      const dark = matchMedia("(prefers-color-scheme: dark)").matches;
      const [catalog, preference, passkeys, shares] = await Promise.all([
        api("/api/v1/themes"), api(themePreferenceURL(dark)), api("/api/v1/me/passkeys"), api("/api/v1/shares"),
      ]);
      state.themes = catalog.themes || [];
      const select = byID("theme-select"); select.replaceChildren();
      const system = text("option", "Follow system"); system.value = "system"; select.append(system);
      for (const theme of state.themes) { const option = text("option", `${theme.name} (${theme.appearance})`); option.value = theme.id; select.append(option); }
      select.value = preference.preference;
      byID("safe-theme").checked = state.safeTheme;
      applyTheme(preference);
      byID("theme-note").textContent = preference.fallback ? "The selected theme was unavailable, so a built-in theme is active." : `Using ${preference.resolved.name}.`;
      renderPasskeys(passkeys.passkeys || []);
      renderShares(shares.shares || []);
    } catch (error) {
      state.passkeys = [];
      state.shares = [];
      byID("passkey-list").replaceChildren(emptyTableRow("Passkeys unavailable", 4));
      byID("share-list").replaceChildren(emptyTableRow("Shares unavailable", 6));
      finishListLoading("passkey-list");
      finishListLoading("share-list");
      announce(friendlyError(error, "Settings could not be loaded."), true);
    }
  }

  function applyTheme(selection) {
    const link = byID("theme-stylesheet");
    if (!selection || !selection.cssURL) {
      link.disabled = true;
      link.removeAttribute("href");
      state.themeAssets = {};
      delete document.documentElement.dataset.theme;
      delete document.documentElement.dataset.appearance;
      document.querySelectorAll(".brand-mark, .entry-mark").forEach((image) => { image.src = "/assets/brand/endlessfs-mark.svg"; delete image.dataset.themeMedia; });
      document.querySelector("link[rel='icon']").href = "/assets/brand/endlessfs-mark.svg";
      return;
    }
    link.href = selection.cssURL;
    link.disabled = false;
    state.themeAssets = selection.assets || {};
    document.documentElement.dataset.theme = selection.resolved.id;
    document.documentElement.dataset.appearance = selection.resolved.appearance;
    const mark = state.themeAssets["brand.mark"];
    const favicon = state.themeAssets["brand.favicon"];
    if (mark && mark.url) {
      document.querySelectorAll(".brand-mark, .entry-mark").forEach((image) => {
        image.src = mark.url;
        image.dataset.themeMedia = "true";
        if (mark.fallbackURL && mark.fallbackURL !== mark.url) image.addEventListener("error", () => { image.src = mark.fallbackURL; }, { once: true });
      });
    }
    if (favicon && favicon.url) document.querySelector("link[rel='icon']").href = favicon.url;
    if (state.user && !byID("drive-view").hidden) renderFiles();
  }

  function renderPasskeys(passkeys) {
    if (passkeys) state.passkeys = passkeys;
    renderVirtualSettingsTable("passkey-table-scroll", "passkey-list", state.passkeys, 4, (passkey) => {
      const row = document.createElement("tr");
      const label = document.createElement("td");
      label.append(text("strong", passkey.label || "Passkey"));
      const added = text("td", formatDate(passkey.createdAt));
      const lastUsed = text("td", formatDate(passkey.lastUsedAt));
      const actions = document.createElement("td");
      actions.className = "table-icon-action-cell";
      const controls = document.createElement("div");
      controls.className = "row-actions";
      controls.append(iconButton("key-off", `Remove ${passkey.label || "passkey"}`, () => removePasskey(passkey), "danger", "Remove passkey"));
      actions.append(controls);
      row.append(label, added, lastUsed, actions);
      return row;
    }, "No passkeys", passkeys !== undefined);
  }

  function renderShares(shares) {
    if (shares) state.shares = shares;
    renderVirtualSettingsTable("share-table-scroll", "share-list", state.shares, 6, (share) => {
      const row = document.createElement("tr");
      const status = share.revokedAt ? "Revoked" : share.expiresAt && new Date(share.expiresAt) <= new Date() ? "Expired" : "Active";
      const location = document.createElement("td");
      location.append(text("strong", share.rootPath));
      const statusCell = text("td", status);
      const kind = text("td", share.kind);
      const created = text("td", formatDate(share.createdAt));
      const expires = text("td", share.expiresAt ? formatDate(share.expiresAt) : "—");
      const actions = document.createElement("td");
      if (status === "Active") {
        actions.className = "table-icon-action-cell";
        const controls = document.createElement("div");
        controls.className = "row-actions";
        controls.append(iconButton("link-off", `Revoke share for ${share.rootPath}`, () => revokeShare(share), "danger", "Revoke share"));
        actions.append(controls);
      }
      row.append(location, statusCell, kind, created, expires, actions);
      return row;
    }, "No public shares", shares !== undefined);
  }

  async function revokeShare(share) {
    const confirmed = await ask({ title: `Revoke share for ${share.rootPath}?`, description: "Anyone using the existing link will immediately see the same generic unavailable state.", confirm: "Revoke Share", danger: true });
    if (!confirmed) return;
    try { await api(`/api/v1/shares/${encodeURIComponent(share.shareID)}`, { method: "DELETE", body: {} }); announce(`Share for ${share.rootPath} revoked.`); await loadSettings(); }
    catch (error) { announce(friendlyError(error), true); }
  }

  async function removePasskey(passkey) {
    const confirmed = await ask({ title: `Remove ${passkey.label || "this passkey"}?`, description: "A recently authenticated session is required, and your final passkey cannot be removed.", confirm: "Remove Passkey", danger: true });
    if (!confirmed) return;
    try { await api(`/api/v1/me/passkeys/${encodeURIComponent(passkey.credentialID)}`, { method: "DELETE" }); announce("Passkey removed."); await loadSettings(); }
    catch (error) { announce(friendlyError(error), true); }
  }

  async function loadAdmin() {
    renderRecordLoadingItems("invite-list");
    renderTableLoadingRows("user-list", "user", 5, 3);
    try {
      const [invites, users] = await Promise.all([api("/api/v1/admin/invites"), api("/api/v1/admin/users?limit=100")]);
      renderInvites(invites.invites || []); renderUsers(users.users || [], false); byID("users-next").hidden = !users.nextCursor; byID("users-next").dataset.cursor = users.nextCursor || "";
    } catch (error) {
      byID("invite-list").replaceChildren();
      byID("user-list").replaceChildren();
      finishListLoading("invite-list");
      finishListLoading("user-list");
      announce(friendlyError(error, "Administration could not be loaded."), true);
    }
  }

  function renderInvites(invites) {
    const list = byID("invite-list"); list.replaceChildren();
    for (const invite of invites) {
      const item = document.createElement("li"); const main = document.createElement("div"); main.className = "record-main";
      const status = invite.revokedAt ? "Revoked" : invite.usedAt ? "Used" : invite.expiresAt && new Date(invite.expiresAt) <= new Date() ? "Expired" : "Available";
      const details = document.createElement("div"); details.append(text("strong", `Invite · ${status}`), text("p", `Created ${formatDate(invite.createdAt)}${invite.expiresAt ? ` · Expires ${formatDate(invite.expiresAt)}` : ""}`, "field-help"));
      main.append(details); if (status === "Available") main.append(iconButton("link-off", "Revoke invite", () => revokeInvite(invite), "danger", "Revoke invite")); item.append(main); list.append(item);
    }
    if (!invites.length) list.append(text("li", "No invites have been created."));
    finishListLoading("invite-list");
  }

  function renderUsers(users, append) {
    const list = byID("user-list"); if (!append) list.replaceChildren();
    for (const user of users) {
      const row = document.createElement("tr");
      row.append(text("td", user.displayName), text("td", user.status), text("td", user.admin ? "Administrator" : "Member"), text("td", formatDate(user.createdAt)));
      const actionCell = document.createElement("td"); actionCell.className = "table-icon-action-cell"; const actions = document.createElement("div"); actions.className = "user-actions";
      actions.append(iconButton(user.status === "enabled" ? "user-off" : "user-check", `${user.status === "enabled" ? "Disable" : "Enable"} ${user.displayName}`, () => adminUserAction(user, user.status === "enabled" ? "disable" : "enable"), `admin-row-action${user.status === "enabled" ? " danger" : ""}`, user.status === "enabled" ? "Disable user" : "Enable user"));
      actions.append(iconButton(user.admin ? "shield-minus" : "shield-plus", user.admin ? `Remove administrator access from ${user.displayName}` : `Make ${user.displayName} an administrator`, () => adminUserAction(user, "admin", user.admin ? "DELETE" : "POST"), `admin-row-action${user.admin ? " danger" : ""}`, user.admin ? "Remove administrator" : "Make administrator"));
      actions.append(iconButton("key", `Create recovery link for ${user.displayName}`, () => createRecovery(user), "admin-row-action", "Create recovery link")); actionCell.append(actions); row.append(actionCell); list.append(row);
    }
    finishListLoading("user-list");
  }

  async function adminUserAction(user, action, method = "POST") {
    const removingAdministrator = action === "admin" && method === "DELETE";
    const title = action === "disable" ? `Disable ${user.displayName}?` :
      action === "enable" ? `Enable ${user.displayName}?` :
      removingAdministrator ? `Remove administrator access from ${user.displayName}?` :
      `Make ${user.displayName} an administrator?`;
    const confirm = action === "disable" ? "Disable" : action === "enable" ? "Enable" : removingAdministrator ? "Remove Administrator" : "Make Administrator";
    const description = action === "disable" ? "This immediately invalidates the user’s active sessions." : action === "admin" ? "Administrator access changes instance-wide controls." : "This changes the user’s account access.";
    const confirmed = await ask({ title, description, confirm, danger: action === "disable" || removingAdministrator });
    if (!confirmed) return;
    try { await api(`/api/v1/admin/users/${encodeURIComponent(user.userID)}/${action}`, { method, body: method === "POST" ? {} : undefined }); announce(`${user.displayName} updated.`); await loadAdmin(); }
    catch (error) { announce(friendlyError(error), true); }
  }

  async function createRecovery(user) {
    const confirmed = await ask({ title: `Create recovery link for ${user.displayName}?`, description: "The one-time link expires in 15 minutes and is shown only once.", confirm: "Create recovery link" });
    if (!confirmed) return;
    try {
      const created = await api(`/api/v1/admin/users/${encodeURIComponent(user.userID)}/recoveries`, { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: { ttlSeconds: 900 } });
      await revealLink("Recovery link", created.link); announce(`Recovery link created for ${user.displayName}.`);
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function revealLink(title, value) {
    const result = await ask({ title, description: "This sensitive link is shown once. Copy it now and share it through a trusted channel.", confirm: "Done", output: value, copyOutput: true });
    return result;
  }

  async function loadPublicShare(append = false) {
    if (append && state.publicLoading) return;
    syncFileBrowserAccess("public");
    showOnly("public-view");
    state.publicLoading = true;
    if (!append) {
      cleanupGridMedia(new Set());
      state.previewStates.clear();
      state.entries = [];
      state.publicCursor = "";
      byID("list-presentation").scrollTop = 0;
      byID("media-grid").scrollTop = 0;
      showState("drive-state", "");
      renderFileLoadingItems();
    }
    const query = new URLSearchParams({ path: state.publicPath, limit: "100" });
    if (append && state.publicCursor) query.set("cursor", state.publicCursor);
    try {
      const page = await api(`/api/v1/public/shares/${encodeURIComponent(state.publicToken)}?${query.toString()}`);
      byID("browser-title").textContent = page.root.name || "Shared files";
      renderBreadcrumbs("breadcrumbs", state.publicPath, (path) => { state.publicPath = path; loadPublicShare(); });
      const entries = page.entries && page.entries.length ? page.entries : (page.root.kind === "file" ? [page.root] : []);
      state.entries = append ? state.entries.concat(entries) : entries;
      state.publicCursor = page.nextCursor || "";
      renderFiles();
    } catch (error) {
      const message = error instanceof APIError && [403, 404, 410].includes(error.status) ? "This share is unavailable. It may have expired, moved, entered trash, or been revoked." : friendlyError(error, "The share could not be loaded.");
      if (!append) {
        state.entries = [];
        byID("file-rows").replaceChildren();
        byID("media-grid-content").replaceChildren();
        finishListLoading("file-rows");
        finishListLoading("media-grid-content");
      }
      showState("drive-state", message, "error");
    } finally {
      state.publicLoading = false;
    }
  }

  function ask(options) {
    const dialog = byID("action-dialog"); const fields = byID("dialog-fields"); fields.replaceChildren();
    byID("dialog-title").textContent = options.title; byID("dialog-description").textContent = options.description || "";
    const confirmLabel = options.confirm || "Continue";
    const confirmControl = byID("dialog-confirm");
    if (options.danger) {
      confirmControl.className = "danger";
      confirmControl.replaceChildren(document.createTextNode(confirmLabel));
      confirmControl.setAttribute("aria-label", confirmLabel);
      delete confirmControl.dataset.icon;
      delete confirmControl.dataset.tooltip;
      confirmControl.removeAttribute("title");
    } else {
      confirmControl.className = "icon-button";
      setIconControl(confirmControl, options.confirmIcon || "check", confirmLabel);
    }
    for (const field of options.fields || []) {
      const label = text("label", field.label); label.htmlFor = field.id;
      const input = document.createElement("input"); input.id = field.id; input.name = field.id; input.type = field.type || "text"; input.value = field.value || ""; input.required = Boolean(field.required); if (field.maxLength) input.maxLength = field.maxLength;
      fields.append(label, input);
    }
    if (options.choiceField) fields.append(selectField(options.choiceField));
    if (options.conflictField) fields.append(selectField({ id: "conflict", label: "If a name is already in use", options: [["fail", "Stop and leave everything unchanged"], ["rename", "Choose a non-conflicting name"], ["replace", "Replace the existing item"]] }));
    if (options.output) { const output = text("output", options.output); output.id = "dialog-output"; fields.append(output); if (options.copyOutput) fields.append(iconButton("copy", "Copy link", () => copyText(options.output))); }
    state.dialogOpener = document.activeElement; showTopLayerDialog(dialog);
    const first = fields.querySelector("input, select, button"); if (first) first.focus(); else byID("dialog-confirm").focus();
    return new Promise((resolve) => {
      const cleanup = () => {
        byID("dialog-cancel").removeEventListener("click", cancel);
        byID("dialog-form").removeEventListener("submit", submit);
        dialog.removeEventListener("cancel", onCancel);
      };
      const finish = (value) => { cleanup(); closeTopLayerDialog(dialog); fields.replaceChildren(); if (state.dialogOpener && state.dialogOpener.focus) state.dialogOpener.focus(); resolve(value); };
      const cancel = () => finish(null);
      const onCancel = (event) => { event.preventDefault(); cancel(); };
      const submit = (event) => {
        event.preventDefault();
        const result = {};
        fields.querySelectorAll("input, select").forEach((control) => { result[control.name] = control.value; });
        finish(result);
      };
      byID("dialog-cancel").addEventListener("click", cancel);
      byID("dialog-form").addEventListener("submit", submit);
      dialog.addEventListener("cancel", onCancel);
    });
  }

  function selectField(field) {
    const wrapper = document.createElement("div"); const label = text("label", field.label); label.htmlFor = field.id;
    const select = document.createElement("select"); select.id = field.id; select.name = field.id;
    for (const [value, labelValue] of field.options) { const option = text("option", labelValue); option.value = value; select.append(option); }
    wrapper.append(label, select); return wrapper;
  }

  async function copyText(value) {
    try { await navigator.clipboard.writeText(value); announce("Link copied."); }
    catch { announce("Copy was unavailable. Select and copy the link manually.", true); }
  }

  function wireEvents() {
    byID("signin-button").addEventListener("click", signIn);
    byID("logout-button").addEventListener("click", logout); byID("settings-logout").addEventListener("click", logout);
    document.querySelectorAll("[data-route]").forEach((link) => link.addEventListener("click", (event) => { if (!state.user) return; event.preventDefault(); setRoute(link.dataset.route); }));
    byID("registration-form").addEventListener("submit", async (event) => {
      event.preventDefault(); const control = event.submitter; control.disabled = true;
      const path = location.pathname; const flow = state.recoveryToken ? "recovery" : state.inviteToken ? "invite" : path === "/bootstrap" ? "bootstrap" : "public";
      const bearer = flow === "bootstrap" ? byID("bootstrap-token").value : flow === "invite" ? state.inviteToken : state.recoveryToken;
      try {
        await register(flow, byID("display-name").value, bearer);
        state.inviteToken = ""; state.recoveryToken = ""; byID("bootstrap-token").value = "";
        announce("Passkey created. You can now sign in."); history.replaceState({}, "", "/"); showOnly("auth-view"); byID("signin-button").focus();
      } catch (error) { announce(friendlyError(error, error.message || "Account setup did not finish."), true); }
      finally { control.disabled = false; }
    });
    byID("new-folder-button").addEventListener("click", createFolder);
    byID("upload-input").addEventListener("change", async (event) => {
      if (state.transferReconnectGroup !== null) await reconnectTransferSources(event.target.files);
      else await queueFiles(event.target.files);
      event.target.value = "";
    });
    byID("folder-input").addEventListener("change", async (event) => {
      if (state.transferReconnectGroup !== null) await reconnectTransferSources(event.target.files);
      else await queueFolderFiles(event.target.files);
      event.target.value = "";
    });
    const drop = byID("drop-target");
    drop.addEventListener("dragover", (event) => { if (state.browserAccess !== "owner") return; event.preventDefault(); drop.classList.add("dragging"); });
    drop.addEventListener("dragleave", () => drop.classList.remove("dragging"));
    drop.addEventListener("drop", async (event) => {
      if (state.browserAccess !== "owner") return;
      event.preventDefault();
      drop.classList.remove("dragging");
      try { await queueDroppedItems(event.dataTransfer); }
      catch (error) { announce(friendlyError(error, "Dropped files could not be read."), true); }
    });
    drop.addEventListener("keydown", (event) => { if (state.browserAccess === "owner" && event.target === drop && (event.key === "Enter" || event.key === " ")) { event.preventDefault(); byID("upload-input").click(); } });
    bindTabs(byID("transfer-filter"), (value) => {
      state.transferFilter = value;
      state.transferVirtualStart = 0;
      renderTransfers();
    });
    const transferConnection = navigator.connection || navigator.mozConnection || navigator.webkitConnection;
    if (transferConnection && transferConnection.addEventListener) transferConnection.addEventListener("change", pumpTransfers);
    byID("clear-transfers").addEventListener("click", async () => {
      const removedGroups = [];
      const removedItems = [];
      for (const [groupID, group] of state.transferGroups) {
        if (["complete", "cancelled"].includes(group.state)) {
          removedGroups.push(groupID);
          removedItems.push(...group.transferIDs);
          for (const id of group.transferIDs) state.transferByID.delete(id);
          state.transferGroups.delete(groupID);
          state.transfers = state.transfers.filter((item) => item.groupID !== groupID);
        }
      }
      state.transfers = state.transfers.filter((item) => {
        const remove = !item.groupID && ["complete", "cancelled"].includes(item.state);
        if (remove) {
          removedItems.push(item.id);
          state.transferByID.delete(item.id);
        }
        return !remove;
      });
      await deleteTransferLedgerEntries(removedItems, removedGroups);
      renderTransfers();
      setTransferSheetOpen(!byID("transfer-panel").hidden);
    });
    byID("retry-failed-transfers").addEventListener("click", retryFailedTransfers);
    byID("transfer-search").addEventListener("input", (event) => {
      state.transferSearch = event.target.value;
      state.transferVirtualStart = 0;
      renderTransfers();
    });
    byID("transfer-list").addEventListener("scroll", () => {
      const list = byID("transfer-list");
      if (list.scrollTop <= transferVirtualRowHeight && state.transferVirtualStart > 0) scheduleTransferWindow(-1);
      else if (list.scrollTop + list.clientHeight >= list.scrollHeight - transferVirtualRowHeight && state.transferVirtualStart + transferVirtualWindowSize < state.transferProjection.length) scheduleTransferWindow(1);
    });
    byID("open-transfers").addEventListener("click", () => setTransferSheetOpen(byID("transfer-panel").hidden, true));
    byID("transfer-close").addEventListener("click", () => setTransferSheetOpen(false, true));
    byID("transfer-panel").addEventListener("keydown", (event) => {
      if (event.key === "Escape") {
        event.preventDefault();
        setTransferSheetOpen(false, true);
        return;
      }
      if (event.key !== "Tab" || !matchMedia("(max-width: 760px)").matches) return;
      const controls = Array.from(byID("transfer-panel").querySelectorAll("button:not(:disabled), input:not(:disabled), [role=tab][tabindex='0']"));
      if (!controls.length) return;
      const first = controls[0];
      const last = controls[controls.length - 1];
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
    });
    byID("file-filter").addEventListener("input", renderFiles); byID("file-sort").addEventListener("change", () => state.browserAccess === "owner" ? loadDirectory(state.currentDirectory) : renderFiles());
    for (const id of ["filter-kind", "filter-media", "filter-min-size", "filter-max-size", "filter-modified-after", "filter-modified-before", "filter-preview"]) byID(id).addEventListener("input", renderFiles);
    for (const id of ["file-view-list", "file-view-grid"]) byID(id).addEventListener("change", (event) => { if (event.target.checked) { state.viewMode = event.target.value; renderFiles(); } });
    byID("media-grid").addEventListener("scroll", () => { if (state.gridRenderFrame) return; state.gridRenderFrame = requestAnimationFrame(() => { state.gridRenderFrame = 0; if (state.viewMode === "grid") renderVirtualGrid(state.filteredEntries); }); });
    byID("list-presentation").addEventListener("scroll", () => {
      if (!state.listRenderFrame) state.listRenderFrame = requestAnimationFrame(() => { state.listRenderFrame = 0; if (state.viewMode === "list") renderVirtualList(state.filteredEntries); });
      const scroller = byID("list-presentation");
      if (scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - (listRowHeight * 6)) {
        if (state.browserAccess === "public" && state.publicCursor && !state.publicLoading) loadPublicShare(true);
        else if (state.browserAccess === "trash" && state.trashCursor && !state.trashLoading) loadTrash(true);
        else if (state.browserAccess === "owner" && state.nextCursor && !state.directoryLoading) loadDirectory(state.currentDirectory, true);
      }
    });
    window.addEventListener("resize", () => {
      if (state.viewMode === "grid") renderVirtualGrid(state.filteredEntries);
      if (!byID("settings-view").hidden) { renderPasskeys(); renderShares(); }
      if (!byID("transfer-panel").hidden) setTransferSheetOpen(true);
    });
    byID("select-all").addEventListener("change", (event) => { for (const entry of filterLoadedEntries(state.entries)) { if (event.target.checked) state.selected.set(entry.path, entry); else state.selected.delete(entry.path); } renderFiles(); updateSelection(); });
    byID("clear-selection").addEventListener("click", () => { state.selected.clear(); renderFiles(); updateSelection(); });
    byID("download-selected").addEventListener("click", () => download([...state.selected.values()][0]));
    byID("share-selected").addEventListener("click", () => createShare([...state.selected.values()][0]));
    byID("copy-selected").addEventListener("click", () => copyMove(false)); byID("move-selected").addEventListener("click", () => copyMove(true)); byID("trash-selected").addEventListener("click", trashSelected);
    byID("next-page").addEventListener("click", () => state.browserAccess === "public" ? loadPublicShare(true) : state.browserAccess === "trash" ? loadTrash(true) : loadDirectory(state.currentDirectory, true)); byID("empty-trash").addEventListener("click", emptyTrash);
    byID("profile-form").addEventListener("submit", async (event) => { event.preventDefault(); try { const profile = await api("/api/v1/me", { method: "PATCH", body: { displayName: byID("profile-name").value } }); state.user.displayName = profile.displayName; announce("Display name saved."); } catch (error) { announce(friendlyError(error), true); } });
    byID("theme-form").addEventListener("submit", async (event) => { event.preventDefault(); try { const dark = matchMedia("(prefers-color-scheme: dark)").matches; state.safeTheme = byID("safe-theme").checked; syncSafeThemeURL(); const selection = state.safeTheme ? await api(themePreferenceURL(dark)) : await api("/api/v1/me/preferences/theme", { method: "PUT", body: { themeID: byID("theme-select").value, dark } }); applyTheme(selection); byID("theme-note").textContent = `Using ${selection.resolved.name}.`; announce("Theme applied."); } catch (error) { announce(friendlyError(error), true); } });
    byID("safe-theme").addEventListener("change", () => byID("theme-form").requestSubmit());
    byID("theme-select").addEventListener("change", () => { byID("safe-theme").checked = false; });
    byID("refresh-shares").addEventListener("click", loadSettings);
    byID("passkey-table-scroll").addEventListener("scroll", () => {
      if (state.passkeyRenderFrame) return;
      state.passkeyRenderFrame = requestAnimationFrame(() => { state.passkeyRenderFrame = 0; renderPasskeys(); });
    });
    byID("share-table-scroll").addEventListener("scroll", () => {
      if (state.shareRenderFrame) return;
      state.shareRenderFrame = requestAnimationFrame(() => { state.shareRenderFrame = 0; renderShares(); });
    });
    byID("add-passkey").addEventListener("click", async () => { const result = await ask({ title: "Add a passkey", description: "Give this passkey an optional device label.", confirm: "Create passkey", confirmIcon: "key-plus", fields: [{ id: "passkey-label", label: "Label (optional)", maxLength: 100 }] }); if (!result) return; try { await register("add", "", "", result["passkey-label"]); announce("Passkey added and session renewed."); await loadSettings(); } catch (error) { announce(friendlyError(error), true); } });
    byID("create-invite").addEventListener("click", async () => { const result = await ask({ title: "Create an invite", description: "The link can create one account. Optionally set an expiry.", confirm: "Create invite", confirmIcon: "user-plus", fields: [{ id: "invite-expires", label: "Expiry (optional)", type: "datetime-local" }] }); if (!result) return; try { const body = {}; if (result["invite-expires"]) body.expiresAt = new Date(result["invite-expires"]).toISOString(); const created = await api("/api/v1/admin/invites", { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body }); await revealLink("Invite link", created.link); announce("Invite created."); await loadAdmin(); } catch (error) { announce(friendlyError(error), true); } });
    byID("copy-invite").addEventListener("click", () => copyText(byID("invite-link").textContent));
    byID("refresh-users").addEventListener("click", loadAdmin);
    byID("users-next").addEventListener("click", async () => { try { const page = await api(`/api/v1/admin/users?limit=100&cursor=${encodeURIComponent(byID("users-next").dataset.cursor)}`); renderUsers(page.users || [], true); byID("users-next").hidden = !page.nextCursor; byID("users-next").dataset.cursor = page.nextCursor || ""; } catch (error) { announce(friendlyError(error), true); } });
    const closePreview = () => { if (state.viewerController) state.viewerController.abort(); state.viewerController = null; releaseViewerObjectURL(); byID("preview-content").replaceChildren(); closeTopLayerDialog(byID("preview-dialog")); if (state.viewerOpener && state.viewerOpener.focus) state.viewerOpener.focus(); state.viewerEntry = null; };
    byID("preview-previous").addEventListener("click", () => navigateViewer(-1));
    byID("preview-next").addEventListener("click", () => navigateViewer(1));
    byID("preview-generate").addEventListener("click", () => generateViewerPreview(false));
    byID("preview-regenerate").addEventListener("click", () => generateViewerPreview(true));
    byID("preview-original").addEventListener("click", () => { if (state.viewerEntry) openSafeOriginalPreview(state.viewerEntry); });
    byID("preview-close").addEventListener("click", closePreview);
    byID("preview-dialog").addEventListener("cancel", (event) => { event.preventDefault(); closePreview(); });
    byID("preview-dialog").addEventListener("keydown", (event) => { if (event.key === "ArrowLeft") { event.preventDefault(); navigateViewer(-1); } if (event.key === "ArrowRight") { event.preventDefault(); navigateViewer(1); } });
    window.addEventListener("popstate", () => { if (state.user) setRoute(routeFromPath(), false); });
    window.addEventListener("online", resumePausedTransfers);
    window.addEventListener("offline", () => {
      for (const transfer of state.transfers.filter((item) => item.state === "retry-wait")) transitionTransfer(transfer, "paused", "Waiting for a network connection.", "offline");
      renderTransfers();
    });
  }

  async function revokeInvite(invite) {
    const confirmed = await ask({ title: "Revoke this invite?", description: "The invite link will stop working immediately.", confirm: "Revoke Invite", danger: true }); if (!confirmed) return;
    try { await api(`/api/v1/admin/invites/${encodeURIComponent(invite.inviteID)}`, { method: "DELETE" }); announce("Invite revoked."); await loadAdmin(); } catch (error) { announce(friendlyError(error), true); }
  }

  function configureRegistration() {
    const bootstrap = location.pathname === "/bootstrap";
    const recovery = location.pathname === "/recover" || Boolean(state.recoveryToken);
    byID("bootstrap-token-field").hidden = !bootstrap;
    byID("bootstrap-token").required = bootstrap;
    byID("display-name-field").hidden = recovery;
    byID("display-name").required = !recovery;
    byID("registration-signin").hidden = bootstrap;
    byID("registration-title").textContent = bootstrap ? "Initialize EndlessFS" : recovery ? "Replace passkey" : state.inviteToken ? "Accept invite" : "Create account";
    byID("registration-submit").textContent = bootstrap ? "Create administrator" : recovery ? "Create replacement passkey" : "Create passkey";
    byID("registration-help").textContent = recovery ? "The recovery link identifies your account. No display name is requested." : "Enter a display name.";
  }

  async function enterApplication() {
    byID("auth-view").hidden = true; byID("registration-view").hidden = true; byID("public-view").hidden = true;
    byID("authenticated-view").hidden = false; byID("loading-view").hidden = false;
    byID("account-actions").hidden = false;
    const admin = (state.user.roles || []).includes("admin"); byID("admin-nav").hidden = !admin;
    const routeLoad = setRoute(routeFromPath(), false);
    const dark = matchMedia("(prefers-color-scheme: dark)").matches;
    const themeLoad = api(themePreferenceURL(dark)).then(applyTheme).catch(() => announce("Your selected theme could not be loaded; a built-in appearance remains active."));
    await Promise.all([routeLoad, themeLoad]);
    await restoreTransferLedger();
    seedTransferPreviewFixture();
    if (document.fonts && document.fonts.ready) await document.fonts.ready;
    byID("loading-view").hidden = true;
    byID("app").dataset.state = "authenticated";
  }

  async function start() {
    consumePathTokens(); wireIconControls(); wireActionTooltips(); wireEvents();
    try { state.config = await api("/api/v1/config"); }
    catch (error) { showState("drive-state", friendlyError(error, "EndlessFS configuration is unavailable."), "error"); }
    if (state.publicToken) { loadPublicShare(); return; }
    if (["/bootstrap", "/register", "/recover"].includes(location.pathname) || state.inviteToken || state.recoveryToken) { configureRegistration(); showOnly("registration-view"); byID("display-name").focus(); return; }
    try { state.user = await api("/api/v1/me"); await enterApplication(); }
    catch (error) {
      if (!(error instanceof APIError) || error.status !== 401) announce(friendlyError(error), true);
      showOnly("auth-view"); byID("account-actions").hidden = true; byID("register-link").hidden = state.config ? !state.config.allowRegistration : false; byID("signin-button").focus();
    }
  }

  window.addEventListener("DOMContentLoaded", start);
})();
