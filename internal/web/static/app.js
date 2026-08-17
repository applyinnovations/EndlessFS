"use strict";

(() => {
  const state = {
    config: null,
    user: null,
    currentDirectory: "/",
    entries: [],
    nextCursor: "",
    selected: new Map(),
    transfers: [],
    activeTransfers: 0,
    themes: [],
    publicToken: "",
    publicPath: "/",
    publicCursor: "",
    inviteToken: "",
    recoveryToken: "",
    dialogOpener: null,
  };

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
  const cookie = (name) => {
    const prefix = `${name}=`;
    const item = document.cookie.split("; ").find((part) => part.startsWith(prefix));
    return item ? decodeURIComponent(item.slice(prefix.length)) : "";
  };
  const csrf = () => cookie("__Host-endlessfs_csrf") || cookie("endlessfs_csrf_dev");
  const idempotencyKey = () => crypto.randomUUID();
  const isMutation = (method) => !["GET", "HEAD"].includes(method);

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
    try { await api("/api/v1/logout", { method: "POST", body: {} }); } catch { /* local transition still clears the UI */ }
    state.user = null;
    state.selected.clear();
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

  function formatDate(value) {
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? "Unknown" : new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(date);
  }

  function setRoute(route, push = true) {
    if (!state.user) return;
    const paths = { drive: "/", trash: "/trash", settings: "/settings", admin: "/admin" };
    if (route === "admin" && !(state.user.roles || []).includes("admin")) route = "drive";
    if (push && location.pathname !== paths[route]) history.pushState({ route }, "", paths[route]);
    for (const name of Object.keys(paths)) byID(`${name}-view`).hidden = name !== route;
    document.querySelectorAll("[data-route]").forEach((link) => {
      if (link.closest(".sidebar")) link.setAttribute("aria-current", link.dataset.route === route ? "page" : "false");
    });
    const heading = byID(`${route}-title`);
    if (heading) heading.focus({ preventScroll: true });
    if (route === "drive") loadDirectory(state.currentDirectory);
    if (route === "trash") loadTrash();
    if (route === "settings") loadSettings();
    if (route === "admin") loadAdmin();
  }

  function routeFromPath() {
    if (location.pathname === "/trash") return "trash";
    if (location.pathname === "/settings") return "settings";
    if (location.pathname === "/admin") return "admin";
    return "drive";
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
    state.currentDirectory = directory;
    if (!append) {
      state.entries = [];
      state.nextCursor = "";
      state.selected.clear();
      updateSelection();
      showState("drive-state", "Loading files…");
    }
    renderBreadcrumbs("breadcrumbs", directory, (path) => loadDirectory(path));
    const [sort, order] = byID("file-sort").value.split(":");
    const cursor = append && state.nextCursor ? `&cursor=${encodeURIComponent(state.nextCursor)}` : "";
    try {
      const page = await api(`/api/v1/files?path=${encodeURIComponent(directory)}&limit=100&sort=${sort}&order=${order}${cursor}`);
      state.entries = append ? state.entries.concat(page.entries || []) : (page.entries || []);
      state.nextCursor = page.nextCursor || "";
      renderFiles();
      announce(`${page.entries.length} item${page.entries.length === 1 ? "" : "s"} loaded from ${pathName(directory)}.`);
    } catch (error) {
      showState("drive-state", friendlyError(error, "Files could not be loaded."), "error");
      byID("file-rows").replaceChildren();
      announce(friendlyError(error), true);
    }
  }

  function renderFiles() {
    const rows = byID("file-rows");
    rows.replaceChildren();
    const query = byID("file-filter").value.trim().toLocaleLowerCase();
    const visible = state.entries.filter((entry) => entry.name.toLocaleLowerCase().includes(query));
    for (const entry of visible) rows.append(fileRow(entry));
    showState("drive-state", visible.length ? "" : (state.entries.length ? "No loaded items match this filter." : "This folder is empty. Upload a file or create a folder."));
    byID("next-page").hidden = !state.nextCursor;
    const selectedVisible = visible.filter((entry) => state.selected.has(entry.path)).length;
    byID("select-all").checked = visible.length > 0 && selectedVisible === visible.length;
    byID("select-all").indeterminate = selectedVisible > 0 && selectedVisible < visible.length;
  }

  function fileRow(entry) {
    const row = document.createElement("tr");
    if (state.selected.has(entry.path)) row.className = "selected";
    const selectCell = document.createElement("td");
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
    const open = button(entry.name, () => entry.kind === "directory" ? loadDirectory(entry.path) : preview(entry), "file-name");
    open.prepend(text("span", entry.kind === "directory" ? "▰" : "▤", "file-icon"));
    open.setAttribute("aria-label", `${entry.kind === "directory" ? "Open folder" : "Preview file"} ${entry.name}`);
    nameCell.append(open);
    const sizeCell = text("td", entry.kind === "directory" ? "Folder" : formatBytes(entry.size));
    const dateCell = text("td", formatDate(entry.modifiedAt));
    const actionCell = document.createElement("td");
    const actions = document.createElement("div");
    actions.className = "row-actions";
    if (entry.kind === "file") actions.append(button("Download", () => download(entry), "quiet"));
    actions.append(button("More", () => openItemActions(entry), "quiet"));
    actionCell.append(actions);
    row.append(selectCell, nameCell, sizeCell, dateCell, actionCell);
    return row;
  }

  function updateSelection() {
    const count = state.selected.size;
    byID("selection-bar").hidden = count === 0;
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

  function queueFiles(files) {
    const items = Array.from(files).filter((file) => file instanceof File);
    if (!items.length) return;
    for (const file of items) {
      const relative = file.webkitRelativePath || file.name;
      const components = relative.split("/").filter(Boolean);
      if (!components.length || components.some((part) => part === "." || part === ".." || part.includes("\\") || part.includes("\u0000"))) {
        announce(`Skipped an invalid upload path for ${file.name}.`, true);
        continue;
      }
      const name = components.pop();
      const relativeDirectory = components.join("/");
      const directory = relativeDirectory ? joinPath(state.currentDirectory, relativeDirectory) : state.currentDirectory;
      state.transfers.push({ id: idempotencyKey(), file, name, directory, relativeDirectory, state: "queued", confirmed: 0, error: "", controller: null, uploadID: "" });
    }
    byID("transfer-panel").hidden = false;
    renderTransfers();
    pumpTransfers();
  }

  async function ensureDirectories(relativeDirectory) {
    if (!relativeDirectory) return;
    let current = state.currentDirectory;
    for (const component of relativeDirectory.split("/")) {
      current = joinPath(current, component);
      try { await api(`/api/v1/files/stat?path=${encodeURIComponent(current)}`); }
      catch (error) {
        if (!(error instanceof APIError) || error.status !== 404) throw error;
        await api("/api/v1/directories", { method: "POST", body: { path: current, conflict: "fail" } });
      }
    }
  }

  function pumpTransfers() {
    const configured = Number.parseInt(byID("transfer-concurrency").value, 10);
    const maximum = state.config ? state.config.maximumTransferConcurrency : 8;
    const concurrency = Math.min(maximum, Math.max(1, Number.isFinite(configured) ? configured : 4));
    byID("transfer-concurrency").value = String(concurrency);
    while (state.activeTransfers < concurrency) {
      const transfer = state.transfers.find((item) => item.state === "queued");
      if (!transfer) break;
      state.activeTransfers += 1;
      uploadTransfer(transfer).finally(() => {
        state.activeTransfers -= 1;
        renderTransfers();
        pumpTransfers();
      });
    }
  }

  async function uploadTransfer(transfer) {
    transfer.state = "preparing";
    transfer.error = "";
    transfer.controller = new AbortController();
    renderTransfers();
    try {
      await ensureDirectories(transfer.relativeDirectory);
      const mediaType = transfer.file.type || "application/octet-stream";
      const capability = await api("/api/v1/uploads", { method: "POST", headers: { "Idempotency-Key": transfer.id }, body: { path: transfer.directory, name: transfer.name, size: transfer.file.size, mediaType, conflict: "rename", resumable: true }, signal: transfer.controller.signal });
      transfer.uploadID = capability.uploadID;
      transfer.state = "uploading";
      renderTransfers();
      await sendFileData(transfer, capability);
      await api(`/api/v1/uploads/${encodeURIComponent(capability.uploadID)}/complete`, { method: "POST", body: { path: joinPath(transfer.directory, transfer.name), size: transfer.file.size, mediaType }, signal: transfer.controller.signal });
      transfer.confirmed = transfer.file.size;
      transfer.state = "complete";
      announce(`${transfer.name} uploaded.`);
      if (transfer.directory === state.currentDirectory) await loadDirectory(state.currentDirectory);
    } catch (error) {
      if (error.name === "AbortError") {
        transfer.state = "cancelled";
        transfer.error = "Cancelled";
      } else {
        transfer.state = "failed";
        transfer.error = friendlyError(error, "Upload interrupted. Retry to resume from a confirmed offset.");
        announce(`${transfer.name} failed to upload.`, true);
      }
    }
  }

  async function sendFileData(transfer, capability) {
    let offset = transfer.confirmed;
    const headersTemplate = capability.headers || {};
    const framing = capability.framing || "offset-header";
    const declaredSize = Number.isSafeInteger(capability.declaredSize) ? capability.declaredSize : transfer.file.size;
    const maximum = capability.chunkRules && capability.chunkRules.maximumSize ? capability.chunkRules.maximumSize : transfer.file.size;
    let failures = 0;
    while (offset < transfer.file.size || (transfer.file.size === 0 && offset === 0)) {
      const end = transfer.file.size === 0 ? 0 : Math.min(transfer.file.size, offset + maximum);
      const headers = new Headers(headersTemplate);
      if (framing === "content-range") {
        headers.delete("Upload-Offset");
        headers.set("Content-Range", transfer.file.size === 0 ? `bytes */${declaredSize}` : `bytes ${offset}-${end - 1}/${declaredSize}`);
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
        transfer.confirmed = offset;
        failures = 0;
        renderTransfers();
        if (transfer.file.size === 0) break;
        continue;
      }
      const status = await api(`/api/v1/uploads/${encodeURIComponent(capability.uploadID)}`);
      offset = status.confirmedOffset;
      transfer.confirmed = offset;
      renderTransfers();
      failures += 1;
      if (failures > 3) throw new Error("Upload interrupted after three recovery attempts.");
      await new Promise((resolve) => window.setTimeout(resolve, 250 * (2 ** (failures - 1)) + Math.floor(Math.random() * 100)));
    }
  }

  async function cancelTransfer(transfer) {
    if (transfer.controller) transfer.controller.abort();
    if (transfer.uploadID) {
      try { await api(`/api/v1/uploads/${encodeURIComponent(transfer.uploadID)}`, { method: "DELETE", body: {} }); } catch { /* already expired or complete */ }
    }
    transfer.state = "cancelled";
    transfer.error = "Cancelled";
    renderTransfers();
    announce(`${transfer.name} cancelled.`);
  }

  function renderTransfers() {
    const list = byID("transfer-list");
    list.replaceChildren();
    for (const transfer of state.transfers) {
      const item = document.createElement("li");
      const row = document.createElement("div");
      row.className = `transfer-row ${transfer.state}`;
      row.append(text("strong", transfer.name), text("span", transfer.state === "uploading" ? `${formatBytes(transfer.confirmed)} of ${formatBytes(transfer.file.size)}` : transfer.state));
      const progress = document.createElement("progress");
      progress.max = Math.max(1, transfer.file.size);
      progress.value = transfer.confirmed;
      progress.setAttribute("aria-label", `Upload progress for ${transfer.name}`);
      row.append(progress);
      if (transfer.error) row.append(text("span", transfer.error, "field-help"));
      if (["queued", "preparing", "uploading"].includes(transfer.state)) row.append(button("Cancel", () => cancelTransfer(transfer), "quiet"));
      if (["failed", "cancelled"].includes(transfer.state)) row.append(button("Retry", () => { transfer.state = "queued"; transfer.error = ""; pumpTransfers(); renderTransfers(); }, "secondary"));
      item.append(row);
      list.append(item);
    }
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
    } catch (error) { announce(friendlyError(error, "Download could not start."), true); }
  }

  async function preview(entry, publicToken = "") {
    if (entry.kind !== "file") return;
    const dialog = byID("preview-dialog");
    byID("preview-title").textContent = entry.name;
    const metadata = byID("preview-metadata");
    metadata.replaceChildren();
    for (const [label, value] of [["Location", entry.path], ["Size", formatBytes(entry.size)], ["Media type", entry.mediaType || "Unknown"], ["Changed", formatDate(entry.modifiedAt)]]) {
      const group = document.createElement("div");
      group.append(text("dt", label), text("dd", value));
      metadata.append(group);
    }
    const content = byID("preview-content");
    content.replaceChildren(text("p", "Preparing a safe preview…", "state-panel"));
    dialog.showModal();
    try {
      const endpoint = publicToken ? `/api/v1/public/shares/${encodeURIComponent(publicToken)}/downloads` : "/api/v1/downloads";
      const created = await api(endpoint, { method: "POST", body: { path: entry.path, version: entry.version, preview: true } });
      content.replaceChildren();
      if (created.mode === "text") {
        const response = await fetch(created.capability.url, { headers: created.capability.headers || {} });
        if (!response.ok) throw new Error("Preview data was unavailable.");
        content.append(text("pre", await response.text()));
      } else if (created.mode === "image") {
        const image = document.createElement("img");
        image.src = created.capability.url;
        image.alt = `Preview of ${entry.name}`;
        content.append(image);
      } else if (created.mode === "pdf") {
        const frame = document.createElement("iframe");
        frame.src = created.capability.url;
        frame.title = `Preview of ${entry.name}`;
        content.append(frame);
      } else {
        content.append(text("p", "A browser preview is not available for this file. Use Download instead.", "state-panel"));
      }
    } catch (error) { content.replaceChildren(text("p", friendlyError(error, "Preview is unavailable."), "state-panel error")); }
  }

  async function openItemActions(entry) {
    const result = await ask({ title: entry.name, description: `${entry.kind === "directory" ? "Folder" : formatBytes(entry.size)} at ${entry.path}`, confirm: "Continue", choiceField: { id: "item-action", label: "Action", options: [["share", "Create public share"], ["copy", "Copy"], ["move", "Move"], ["trash", "Move to trash"]] } });
    if (!result) return;
    state.selected.clear();
    state.selected.set(entry.path, entry);
    if (result["item-action"] === "share") createShare(entry);
    if (result["item-action"] === "copy") copyMove(false);
    if (result["item-action"] === "move") copyMove(true);
    if (result["item-action"] === "trash") trashSelected();
  }

  async function copyMove(move) {
    const entries = [...state.selected.values()];
    const result = await ask({ title: `${move ? "Move" : "Copy"} ${entries.length} item${entries.length === 1 ? "" : "s"}`, description: "Enter a destination folder path. Names are kept unchanged.", confirm: move ? "Move" : "Copy", fields: [{ id: "destination", label: "Destination folder", value: state.currentDirectory, required: true }], conflictField: true });
    if (!result) return;
    const items = entries.map((entry) => ({ source: entry.path, destination: joinPath(result.destination.replace(/\/$/, "") || "/", entry.name), conflict: result.conflict }));
    try {
      const endpoint = move ? "/api/v1/files/move" : "/api/v1/files/copy";
      const operation = await api(endpoint, { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: items.length === 1 ? items[0] : { items } });
      announce(`${move ? "Move" : "Copy"} started for ${items.length} item${items.length === 1 ? "" : "s"}.`);
      state.selected.clear(); updateSelection();
      await watchOperation(operation.operationID || operation.id);
      await loadDirectory(state.currentDirectory);
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function trashSelected() {
    const paths = [...state.selected.keys()];
    if (!paths.length) return;
    const confirmed = await ask({ title: `Move ${paths.length} item${paths.length === 1 ? "" : "s"} to trash?`, description: "The items leave the current folder but can be restored from Trash.", confirm: "Move to trash", danger: true });
    if (!confirmed) return;
    try {
      const result = await api("/api/v1/files/trash", { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: { paths } });
      announce(`${paths.length} item${paths.length === 1 ? "" : "s"} moved to trash.`);
      state.selected.clear(); updateSelection();
      if (result.operationID) await watchOperation(result.operationID);
      await loadDirectory(state.currentDirectory);
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
    if (!append) { showState("trash-state", "Loading trash…"); byID("trash-rows").replaceChildren(); }
    try {
      const cursor = append && byID("trash-next").dataset.cursor ? `&cursor=${encodeURIComponent(byID("trash-next").dataset.cursor)}` : "";
      const page = await api(`/api/v1/trash?limit=100${cursor}`);
      if (!append) byID("trash-rows").replaceChildren();
      for (const item of page.items || []) byID("trash-rows").append(trashRow(item));
      const empty = !byID("trash-rows").children.length;
      showState("trash-state", empty ? "Trash is empty." : "");
      byID("trash-next").hidden = !page.nextCursor;
      byID("trash-next").dataset.cursor = page.nextCursor || "";
      byID("empty-trash").disabled = empty;
    } catch (error) { showState("trash-state", friendlyError(error, "Trash could not be loaded."), "error"); }
  }

  function trashRow(item) {
    const row = document.createElement("tr");
    row.append(text("td", item.originalPath), text("td", item.kind), text("td", formatDate(item.trashedAt)));
    const actionCell = document.createElement("td");
    const actions = document.createElement("div"); actions.className = "row-actions";
    actions.append(button("Restore", () => restoreTrash(item), "secondary"), button("Delete forever", () => deleteTrash(item), "danger-quiet"));
    actionCell.append(actions); row.append(actionCell); return row;
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
    const confirmed = await ask({ title: `Permanently delete ${item.originalPath}?`, description: "This cannot be undone. The file data will be permanently removed.", confirm: "Delete permanently", danger: true });
    if (!confirmed) return;
    try {
      const operation = await api(`/api/v1/trash/${encodeURIComponent(item.trashID)}`, { method: "DELETE", headers: { "Idempotency-Key": idempotencyKey() }, body: {} });
      await watchOperation(operation.id || operation.operationID); announce(`${item.originalPath} permanently deleted.`); await loadTrash();
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function emptyTrash() {
    const confirmed = await ask({ title: "Permanently delete every trashed item?", description: "All items in Trash will be removed. This cannot be undone.", confirm: "Empty trash permanently", danger: true });
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
    try {
      const dark = matchMedia("(prefers-color-scheme: dark)").matches;
      const [catalog, preference, passkeys, shares] = await Promise.all([
        api("/api/v1/themes"), api(`/api/v1/me/preferences/theme?dark=${dark}`), api("/api/v1/me/passkeys"), api("/api/v1/shares"),
      ]);
      state.themes = catalog.themes || [];
      const select = byID("theme-select"); select.replaceChildren();
      const system = text("option", "Follow system"); system.value = "system"; select.append(system);
      for (const theme of state.themes) { const option = text("option", `${theme.name} (${theme.appearance})`); option.value = theme.id; select.append(option); }
      select.value = preference.preference;
      applyTheme(preference);
      byID("theme-note").textContent = preference.fallback ? "The selected theme was unavailable, so a built-in theme is active." : `Using ${preference.resolved.name}.`;
      renderPasskeys(passkeys.passkeys || []);
      renderShares(shares.shares || []);
    } catch (error) { announce(friendlyError(error, "Settings could not be loaded."), true); }
  }

  function applyTheme(selection) {
    const link = byID("theme-stylesheet");
    if (!selection || !selection.cssURL) { link.disabled = true; link.removeAttribute("href"); return; }
    link.href = selection.cssURL;
    link.disabled = false;
    document.documentElement.dataset.theme = selection.resolved.id;
  }

  function renderPasskeys(passkeys) {
    const list = byID("passkey-list"); list.replaceChildren();
    for (const passkey of passkeys) {
      const item = document.createElement("li"); const main = document.createElement("div"); main.className = "record-main";
      const details = document.createElement("div"); details.append(text("strong", passkey.label || "Passkey"), text("p", `Added ${formatDate(passkey.createdAt)} · Last used ${formatDate(passkey.lastUsedAt)}`, "field-help"));
      main.append(details, button("Remove", () => removePasskey(passkey), "danger-quiet")); item.append(main); list.append(item);
    }
  }

  function renderShares(shares) {
    const list = byID("share-list"); list.replaceChildren();
    for (const share of shares) {
      const item = document.createElement("li"); const main = document.createElement("div"); main.className = "record-main";
      const details = document.createElement("div");
      const status = share.revokedAt ? "Revoked" : share.expiresAt && new Date(share.expiresAt) <= new Date() ? "Expired" : "Active";
      details.append(text("strong", `${share.rootPath} · ${status}`), text("p", `${share.kind} share created ${formatDate(share.createdAt)}${share.expiresAt ? ` · Expires ${formatDate(share.expiresAt)}` : ""}`, "field-help"));
      main.append(details);
      if (status === "Active") main.append(button("Revoke", () => revokeShare(share), "danger-quiet"));
      item.append(main); list.append(item);
    }
    if (!shares.length) list.append(text("li", "No public shares."));
  }

  async function revokeShare(share) {
    const confirmed = await ask({ title: `Revoke share for ${share.rootPath}?`, description: "Anyone using the existing link will immediately see the same generic unavailable state.", confirm: "Revoke share", danger: true });
    if (!confirmed) return;
    try { await api(`/api/v1/shares/${encodeURIComponent(share.shareID)}`, { method: "DELETE", body: {} }); announce(`Share for ${share.rootPath} revoked.`); await loadSettings(); }
    catch (error) { announce(friendlyError(error), true); }
  }

  async function removePasskey(passkey) {
    const confirmed = await ask({ title: `Remove ${passkey.label || "this passkey"}?`, description: "A recently authenticated session is required, and your final passkey cannot be removed.", confirm: "Remove passkey", danger: true });
    if (!confirmed) return;
    try { await api(`/api/v1/me/passkeys/${encodeURIComponent(passkey.credentialID)}`, { method: "DELETE" }); announce("Passkey removed."); await loadSettings(); }
    catch (error) { announce(friendlyError(error), true); }
  }

  async function loadAdmin() {
    try {
      const [invites, users] = await Promise.all([api("/api/v1/admin/invites"), api("/api/v1/admin/users?limit=100")]);
      renderInvites(invites.invites || []); renderUsers(users.users || [], false); byID("users-next").hidden = !users.nextCursor; byID("users-next").dataset.cursor = users.nextCursor || "";
    } catch (error) { announce(friendlyError(error, "Administration could not be loaded."), true); }
  }

  function renderInvites(invites) {
    const list = byID("invite-list"); list.replaceChildren();
    for (const invite of invites) {
      const item = document.createElement("li"); const main = document.createElement("div"); main.className = "record-main";
      const status = invite.revokedAt ? "Revoked" : invite.usedAt ? "Used" : invite.expiresAt && new Date(invite.expiresAt) <= new Date() ? "Expired" : "Available";
      const details = document.createElement("div"); details.append(text("strong", `Invite · ${status}`), text("p", `Created ${formatDate(invite.createdAt)}${invite.expiresAt ? ` · Expires ${formatDate(invite.expiresAt)}` : ""}`, "field-help"));
      main.append(details); if (status === "Available") main.append(button("Revoke", () => revokeInvite(invite), "danger-quiet")); item.append(main); list.append(item);
    }
    if (!invites.length) list.append(text("li", "No invites have been created."));
  }

  function renderUsers(users, append) {
    const list = byID("user-list"); if (!append) list.replaceChildren();
    for (const user of users) {
      const item = document.createElement("li"); const main = document.createElement("div"); main.className = "record-main";
      const details = document.createElement("div"); details.append(text("strong", user.displayName), text("p", `${user.status}${user.admin ? " · Administrator" : ""} · Added ${formatDate(user.createdAt)}`, "field-help"));
      const actions = document.createElement("div"); actions.className = "record-actions";
      actions.append(button(user.status === "enabled" ? "Disable" : "Enable", () => adminUserAction(user, user.status === "enabled" ? "disable" : "enable"), user.status === "enabled" ? "danger-quiet" : "secondary"));
      actions.append(button(user.admin ? "Remove admin" : "Make admin", () => adminUserAction(user, "admin", user.admin ? "DELETE" : "POST"), "secondary"));
      actions.append(button("Recovery link", () => createRecovery(user), "secondary")); main.append(details, actions); item.append(main); list.append(item);
    }
  }

  async function adminUserAction(user, action, method = "POST") {
    const description = action === "disable" ? "This immediately invalidates the user’s active sessions." : action === "admin" ? "Administrator access changes instance-wide controls." : "This changes the user’s account access.";
    const confirmed = await ask({ title: `${action === "admin" && method === "DELETE" ? "Remove administrator access from" : action} ${user.displayName}?`, description, confirm: "Confirm change", danger: action === "disable" || method === "DELETE" });
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
    showOnly("public-view");
    if (!append) { state.publicCursor = ""; showState("public-state", "Loading shared items…"); byID("public-rows").replaceChildren(); }
    const query = new URLSearchParams({ path: state.publicPath, limit: "100" });
    if (append && state.publicCursor) query.set("cursor", state.publicCursor);
    try {
      const page = await api(`/api/v1/public/shares/${encodeURIComponent(state.publicToken)}?${query.toString()}`);
      byID("public-title").textContent = page.root.name || "Shared files";
      renderBreadcrumbs("public-breadcrumbs", state.publicPath, (path) => { state.publicPath = path; loadPublicShare(); });
      if (!append) byID("public-rows").replaceChildren();
      const entries = page.entries && page.entries.length ? page.entries : (page.root.kind === "file" ? [page.root] : []);
      for (const entry of entries) byID("public-rows").append(publicRow(entry));
      showState("public-state", byID("public-rows").children.length ? "" : "This shared folder is empty.");
      state.publicCursor = page.nextCursor || ""; byID("public-next").hidden = !state.publicCursor;
    } catch (error) {
      const message = error instanceof APIError && [403, 404, 410].includes(error.status) ? "This share is unavailable. It may have expired, moved, entered trash, or been revoked." : friendlyError(error, "The share could not be loaded.");
      showState("public-state", message, "error"); byID("public-table").hidden = true;
    }
  }

  function publicRow(entry) {
    const row = document.createElement("tr"); const nameCell = document.createElement("td");
    const action = button(entry.name, () => { if (entry.kind === "directory") { state.publicPath = entry.path; loadPublicShare(); } else preview(entry, state.publicToken); }, "file-name");
    action.prepend(text("span", entry.kind === "directory" ? "▰" : "▤", "file-icon")); nameCell.append(action);
    row.append(nameCell, text("td", entry.kind === "directory" ? "Folder" : formatBytes(entry.size)), text("td", formatDate(entry.modifiedAt)));
    const actions = document.createElement("td"); if (entry.kind === "file") actions.append(button("Download", () => download(entry, state.publicToken), "secondary")); row.append(actions); return row;
  }

  function ask(options) {
    const dialog = byID("action-dialog"); const fields = byID("dialog-fields"); fields.replaceChildren();
    byID("dialog-title").textContent = options.title; byID("dialog-description").textContent = options.description || "";
    byID("dialog-confirm").textContent = options.confirm || "Continue"; byID("dialog-confirm").className = options.danger ? "danger" : "primary";
    for (const field of options.fields || []) {
      const label = text("label", field.label); label.htmlFor = field.id;
      const input = document.createElement("input"); input.id = field.id; input.name = field.id; input.type = field.type || "text"; input.value = field.value || ""; input.required = Boolean(field.required); if (field.maxLength) input.maxLength = field.maxLength;
      fields.append(label, input);
    }
    if (options.choiceField) fields.append(selectField(options.choiceField));
    if (options.conflictField) fields.append(selectField({ id: "conflict", label: "If a name is already in use", options: [["fail", "Stop and leave everything unchanged"], ["rename", "Choose a non-conflicting name"], ["replace", "Replace the existing item"]] }));
    if (options.output) { const output = text("output", options.output); output.id = "dialog-output"; fields.append(output); if (options.copyOutput) fields.append(button("Copy link", () => copyText(options.output), "secondary")); }
    state.dialogOpener = document.activeElement; dialog.showModal();
    const first = fields.querySelector("input, select, button"); if (first) first.focus(); else byID("dialog-confirm").focus();
    return new Promise((resolve) => {
      const cleanup = () => {
        byID("dialog-cancel").removeEventListener("click", cancel);
        byID("dialog-form").removeEventListener("submit", submit);
        dialog.removeEventListener("cancel", onCancel);
      };
      const finish = (value) => { cleanup(); dialog.close(); fields.replaceChildren(); if (state.dialogOpener && state.dialogOpener.focus) state.dialogOpener.focus(); resolve(value); };
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
    byID("upload-input").addEventListener("change", (event) => { queueFiles(event.target.files); event.target.value = ""; });
    byID("folder-input").addEventListener("change", (event) => { queueFiles(event.target.files); event.target.value = ""; });
    const drop = byID("drop-target");
    drop.addEventListener("dragover", (event) => { event.preventDefault(); drop.classList.add("dragging"); });
    drop.addEventListener("dragleave", () => drop.classList.remove("dragging"));
    drop.addEventListener("drop", (event) => { event.preventDefault(); drop.classList.remove("dragging"); queueFiles(event.dataTransfer.files); });
    drop.addEventListener("keydown", (event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); byID("upload-input").click(); } });
    byID("transfer-concurrency").addEventListener("change", pumpTransfers);
    byID("clear-transfers").addEventListener("click", () => { state.transfers = state.transfers.filter((item) => !["complete", "cancelled"].includes(item.state)); renderTransfers(); byID("transfer-panel").hidden = !state.transfers.length; });
    byID("file-filter").addEventListener("input", renderFiles); byID("file-sort").addEventListener("change", () => loadDirectory(state.currentDirectory));
    byID("select-all").addEventListener("change", (event) => { const query = byID("file-filter").value.trim().toLocaleLowerCase(); for (const entry of state.entries.filter((item) => item.name.toLocaleLowerCase().includes(query))) { if (event.target.checked) state.selected.set(entry.path, entry); else state.selected.delete(entry.path); } renderFiles(); updateSelection(); });
    byID("clear-selection").addEventListener("click", () => { state.selected.clear(); renderFiles(); updateSelection(); });
    byID("download-selected").addEventListener("click", () => download([...state.selected.values()][0]));
    byID("share-selected").addEventListener("click", () => createShare([...state.selected.values()][0]));
    byID("copy-selected").addEventListener("click", () => copyMove(false)); byID("move-selected").addEventListener("click", () => copyMove(true)); byID("trash-selected").addEventListener("click", trashSelected);
    byID("next-page").addEventListener("click", () => loadDirectory(state.currentDirectory, true)); byID("trash-next").addEventListener("click", () => loadTrash(true)); byID("empty-trash").addEventListener("click", emptyTrash);
    byID("profile-form").addEventListener("submit", async (event) => { event.preventDefault(); try { const profile = await api("/api/v1/me", { method: "PATCH", body: { displayName: byID("profile-name").value } }); state.user.displayName = profile.displayName; byID("account-name").textContent = profile.displayName; announce("Display name saved."); } catch (error) { announce(friendlyError(error), true); } });
    byID("theme-form").addEventListener("submit", async (event) => { event.preventDefault(); try { const dark = matchMedia("(prefers-color-scheme: dark)").matches; const selection = byID("safe-theme").checked ? await api(`/api/v1/me/preferences/theme?dark=${dark}&safe-theme=1`) : await api("/api/v1/me/preferences/theme", { method: "PUT", body: { themeID: byID("theme-select").value, dark } }); applyTheme(selection); byID("theme-note").textContent = `Using ${selection.resolved.name}.`; announce("Theme applied."); } catch (error) { announce(friendlyError(error), true); } });
    byID("safe-theme").addEventListener("change", () => byID("theme-form").requestSubmit());
    byID("refresh-shares").addEventListener("click", loadSettings);
    byID("add-passkey").addEventListener("click", async () => { const result = await ask({ title: "Add a passkey", description: "Give this passkey an optional device label.", confirm: "Create passkey", fields: [{ id: "passkey-label", label: "Label (optional)", maxLength: 100 }] }); if (!result) return; try { await register("add", "", "", result["passkey-label"]); announce("Passkey added and session renewed."); await loadSettings(); } catch (error) { announce(friendlyError(error), true); } });
    byID("create-invite").addEventListener("click", async () => { const result = await ask({ title: "Create an invite", description: "The link can create one account. Optionally set an expiry.", confirm: "Create invite", fields: [{ id: "invite-expires", label: "Expiry (optional)", type: "datetime-local" }] }); if (!result) return; try { const body = {}; if (result["invite-expires"]) body.expiresAt = new Date(result["invite-expires"]).toISOString(); const created = await api("/api/v1/admin/invites", { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body }); await revealLink("Invite link", created.link); announce("Invite created."); await loadAdmin(); } catch (error) { announce(friendlyError(error), true); } });
    byID("copy-invite").addEventListener("click", () => copyText(byID("invite-link").textContent));
    byID("refresh-users").addEventListener("click", loadAdmin);
    byID("users-next").addEventListener("click", async () => { try { const page = await api(`/api/v1/admin/users?limit=100&cursor=${encodeURIComponent(byID("users-next").dataset.cursor)}`); renderUsers(page.users || [], true); byID("users-next").hidden = !page.nextCursor; byID("users-next").dataset.cursor = page.nextCursor || ""; } catch (error) { announce(friendlyError(error), true); } });
    byID("public-next").addEventListener("click", () => loadPublicShare(true));
    const closePreview = () => { byID("preview-content").replaceChildren(); byID("preview-dialog").close(); };
    byID("preview-close").addEventListener("click", closePreview);
    byID("preview-dialog").addEventListener("cancel", (event) => { event.preventDefault(); closePreview(); });
    window.addEventListener("popstate", () => { if (state.user) setRoute(routeFromPath(), false); });
    window.addEventListener("online", () => { byID("connection-status").textContent = "Online"; byID("connection-status").classList.remove("offline"); announce("Connection restored."); });
    window.addEventListener("offline", () => { byID("connection-status").textContent = "Offline"; byID("connection-status").classList.add("offline"); announce("You are offline. Active transfers may pause.", true); });
  }

  async function revokeInvite(invite) {
    const confirmed = await ask({ title: "Revoke this invite?", description: "The invite link will stop working immediately.", confirm: "Revoke invite", danger: true }); if (!confirmed) return;
    try { await api(`/api/v1/admin/invites/${encodeURIComponent(invite.inviteID)}`, { method: "DELETE" }); announce("Invite revoked."); await loadAdmin(); } catch (error) { announce(friendlyError(error), true); }
  }

  function configureRegistration() {
    const bootstrap = location.pathname === "/bootstrap";
    const recovery = location.pathname === "/recover" || Boolean(state.recoveryToken);
    byID("bootstrap-token-field").hidden = !bootstrap;
    byID("bootstrap-token").required = bootstrap;
    if (bootstrap) { byID("registration-eyebrow").textContent = "Instance setup"; byID("registration-title").textContent = "Create the first administrator"; }
    else if (recovery) { byID("registration-eyebrow").textContent = "Account recovery"; byID("registration-title").textContent = "Create a replacement passkey"; byID("registration-help").textContent = "The recovery link identifies your account. No display name is requested."; byID("display-name").closest("form").querySelector("label[for='display-name']").hidden = true; byID("display-name").hidden = true; byID("display-name").required = false; }
    else if (state.inviteToken) { byID("registration-eyebrow").textContent = "Invited registration"; byID("registration-title").textContent = "Accept your invite"; }
  }

  function enterApplication() {
    showOnly("authenticated-view"); byID("account-actions").hidden = false; byID("account-name").textContent = state.user.displayName;
    const admin = (state.user.roles || []).includes("admin"); byID("admin-nav").hidden = !admin;
    byID("connection-status").textContent = navigator.onLine ? "Online" : "Offline";
    setRoute(routeFromPath(), false);
    const dark = matchMedia("(prefers-color-scheme: dark)").matches;
    api(`/api/v1/me/preferences/theme?dark=${dark}`).then(applyTheme).catch(() => announce("Your selected theme could not be loaded; a built-in appearance remains active."));
  }

  async function start() {
    consumePathTokens(); wireEvents();
    try { state.config = await api("/api/v1/config"); byID("transfer-concurrency").value = String(state.config.defaultTransferConcurrency || 4); }
    catch (error) { showState("drive-state", friendlyError(error, "EndlessFS configuration is unavailable."), "error"); byID("connection-status").textContent = "Unavailable"; }
    if (state.publicToken) { loadPublicShare(); return; }
    if (["/bootstrap", "/register", "/recover"].includes(location.pathname) || state.inviteToken || state.recoveryToken) { configureRegistration(); showOnly("registration-view"); byID("display-name").focus(); return; }
    try { state.user = await api("/api/v1/me"); enterApplication(); }
    catch (error) {
      if (!(error instanceof APIError) || error.status !== 401) announce(friendlyError(error), true);
      showOnly("auth-view"); byID("account-actions").hidden = true; byID("register-link").hidden = state.config ? !state.config.allowRegistration : false; byID("signin-button").focus();
    }
  }

  window.addEventListener("DOMContentLoaded", start);
})();
