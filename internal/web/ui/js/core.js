"use strict";

(() => {
  const state = {
    config: null,
    user: null,
    currentDirectory: "/",
    currentEntry: null,
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
    invites: [],
    users: [],
    usersCursor: "",
    usersLoading: false,
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
    inviteRenderFrame: 0,
    userRenderFrame: 0,
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
  const transferVirtualRowHeight = 60;
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
    "menu-2": ["M4 6h16", "M4 12h16", "M4 18h16"],
    "message-report": ["M18 4a3 3 0 0 1 3 3v8a3 3 0 0 1 -3 3h-5l-5 3v-3h-2a3 3 0 0 1 -3 -3v-8a3 3 0 0 1 3 -3h12", "M12 8v3", "M12 14v.01"],
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
    syncBrowserPagingSentinels();
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

  function dismissToastButton() {
    return iconButton("x", "Dismiss notification", clearToast, "toast-dismiss", "Dismiss");
  }

  function showToast(message, level, duration = 8000) {
    clearToast();
    const toast = document.createElement("div");
    toast.className = `toast ${level}`;
    toast.append(text("span", message), dismissToastButton());
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
    const actions = document.createElement("div");
    actions.className = "toast-actions";
    actions.append(undo, dismissToastButton());
    toast.append(actions);
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
    const accessChanged = state.browserAccess !== access;
    if (surface.parentElement !== host) host.append(surface);
    state.browserAccess = access;
    if (accessChanged) { state.selected.clear(); updateSelection(); }
    surface.dataset.access = access;
    byID("breadcrumbs").setAttribute("aria-label", access === "public" ? "Shared folder path" : "Current folder");
    const collectionLabel = access === "trash" ? "Items in trash" : access === "public" ? "Files in shared folder" : "Files in the current folder";
    byID("file-table-caption").textContent = collectionLabel;
    byID("media-grid").setAttribute("aria-label", collectionLabel);
    byID("select-all").setAttribute("aria-label", access === "trash" ? "Select all trashed items on this page" : "Select all files on this page");
    byID("selection-bar").setAttribute("aria-label", access === "trash" ? "Trash selection actions" : "Selection actions");
    if (access !== "trash") state.trashRequest += 1;
    if (access !== "owner") {
      state.directoryRequest += 1;
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
      if (link.closest(".app-tabs, .mobile-navigation")) link.setAttribute("aria-current", link.dataset.route === route ? "page" : "false");
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
