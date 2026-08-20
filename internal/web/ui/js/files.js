  const fileDetailsByMediaType = Object.freeze({
    "application/pdf": "PDF document",
    "application/json": "JSON document",
    "application/zip": "ZIP archive",
    "image/gif": "GIF image",
    "image/jpeg": "JPEG image",
    "image/png": "PNG image",
    "image/svg+xml": "SVG image",
    "image/webp": "WebP image",
    "text/csv": "CSV document",
    "text/html": "HTML document",
    "text/plain": "Plain text",
  });

  async function loadDirectory(directory, append = false, preserveSelection = false) {
    if (append && state.directoryLoading) return;
    const request = state.directoryRequest + 1;
    state.directoryRequest = request;
    state.directoryLoading = true;
    state.currentDirectory = directory;
    if (!append) {
      cleanupGridMedia(new Set());
      state.previewStates.clear();
      state.currentEntry = null;
      renderPathAggregate(null);
      state.entries = [];
      state.nextCursor = "";
      if (!preserveSelection) state.selected.clear();
      byID("list-presentation").scrollTop = 0;
      byID("media-grid").scrollTop = 0;
      updateSelection();
      showState("drive-state", "");
      renderFileLoadingItems();
    }
    renderBreadcrumbs("breadcrumbs", directory, (path) => loadDirectory(path));
    const [sort, order] = (state.viewMode === "storage" ? "size:desc" : byID("file-sort").value).split(":");
    const limit = `limit=${state.viewMode === "storage" ? storageMapPageSize : 100}`;
    const cursor = append && state.nextCursor ? `&cursor=${encodeURIComponent(state.nextCursor)}` : "";
    try {
      const page = await api(`/api/v1/files?path=${encodeURIComponent(directory)}&${limit}&sort=${sort}&order=${order}${cursor}`);
      if (request !== state.directoryRequest) return;
      state.currentEntry = page.current;
      renderPathAggregate(page.current);
      state.entries = append ? state.entries.concat(page.entries || []) : (page.entries || []);
      state.nextCursor = page.nextCursor || "";
      renderFiles();
      announce(`${page.entries.length} item${page.entries.length === 1 ? "" : "s"} loaded from ${pathName(directory)}.`);
    } catch (error) {
      if (request !== state.directoryRequest) return;
      if (append) announce(friendlyError(error, "More files could not be loaded."), true);
      else {
        state.currentEntry = null;
        renderPathAggregate(null);
        showState("drive-state", friendlyError(error, "Files could not be loaded."), "error");
        byID("file-rows").replaceChildren();
        byID("media-grid-content").replaceChildren();
        finishListLoading("file-rows");
        finishListLoading("media-grid-content");
        announce(friendlyError(error), true);
      }
    } finally {
      if (request === state.directoryRequest) {
        state.directoryLoading = false;
        requestAnimationFrame(() => maybeLoadNextBrowserPage(state.viewMode === "grid" ? byID("media-grid") : byID("list-presentation")));
      }
    }
  }

  function renderPathAggregate(entry) {
    const aggregate = byID("path-aggregate");
    if (state.browserAccess === "trash") {
      aggregate.textContent = "";
      aggregate.removeAttribute("aria-label");
      aggregate.removeAttribute("aria-hidden");
      aggregate.classList.remove("pending");
      aggregate.hidden = true;
      return;
    }
    aggregate.hidden = false;
    if (!entry) {
      aggregate.textContent = "— · —";
      aggregate.removeAttribute("aria-label");
      aggregate.setAttribute("aria-hidden", "true");
      aggregate.classList.add("pending");
      return;
    }
    const size = formatEntrySize(entry);
    const fileCount = formatEntryFileCount(entry);
    aggregate.textContent = `${size} · ${fileCount}`;
    aggregate.setAttribute("aria-label", `Current path total: ${size}, ${fileCount}`);
    aggregate.removeAttribute("aria-hidden");
    aggregate.classList.remove("pending");
  }

  function knownEntrySize(entry) {
    const value = entry && entry.size;
    return Number.isSafeInteger(value) && value >= 0 ? value : null;
  }

  function knownEntryFileCount(entry) {
    const value = entry && entry.fileCount;
    return Number.isSafeInteger(value) && value >= 0 ? value : null;
  }

  function formatEntrySize(entry) {
    const size = knownEntrySize(entry);
    return size === null ? "—" : formatBytes(size);
  }

  function formatEntryFileCount(entry) {
    const fileCount = knownEntryFileCount(entry);
    return fileCount === null ? "—" : `${fileCount.toLocaleString()} ${fileCount === 1 ? "file" : "files"}`;
  }

  function entrySelectionKey(entry) {
    return entry && entry.trash && entry.trash.trashID ? `trash:${entry.trash.trashID}` : entry.path;
  }

  function formatEntryDetails(entry) {
    if (entry.kind === "directory") return formatEntryFileCount(entry);
    const mediaType = (entry.mediaType || "").toLocaleLowerCase();
    if (fileDetailsByMediaType[mediaType]) return fileDetailsByMediaType[mediaType];
    const categories = { image: "Image", video: "Video", audio: "Audio", document: "Document", archive: "Archive" };
    return categories[mediaCategory(entry)] || "File";
  }

  function compareEntrySizes(left, right, direction) {
    const leftSize = knownEntrySize(left);
    const rightSize = knownEntrySize(right);
    if (leftSize === null) return rightSize === null ? 0 : 1;
    if (rightSize === null) return -1;
    if (leftSize === rightSize) return 0;
    return direction * (leftSize < rightSize ? -1 : 1);
  }

  function renderFiles() {
    const visible = filterLoadedEntries(state.entries);
    state.filteredEntries = visible;
    finishListLoading("file-rows");
    finishListLoading("media-grid-content");
    const gridEnabled = syncFilePresentation();
    if (state.viewMode === "storage" && state.browserAccess === "owner") {
      cleanupGridMedia(new Set());
      renderStorageMap(visible);
    } else if (gridEnabled) renderVirtualGrid(visible);
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
    syncBrowserPagingSentinels();
    const selectionAccess = state.browserAccess === "owner" || state.browserAccess === "trash";
    if (selectionAccess) {
      const selectedVisible = visible.filter((entry) => state.selected.has(entrySelectionKey(entry))).length;
      byID("select-all").checked = visible.length > 0 && selectedVisible === visible.length;
      byID("select-all").indeterminate = selectedVisible > 0 && selectedVisible < visible.length;
    }
  }

  function browserNextCursor() {
    if (state.browserAccess === "public") return state.publicCursor;
    if (state.browserAccess === "trash") return state.trashCursor;
    return state.nextCursor;
  }

  function browserPageLoading() {
    if (state.browserAccess === "public") return state.publicLoading;
    if (state.browserAccess === "trash") return state.trashLoading;
    return state.directoryLoading;
  }

  function loadNextBrowserPage() {
    if (!browserNextCursor() || browserPageLoading()) return;
    if (state.browserAccess === "public") loadPublicShare(true);
    else if (state.browserAccess === "trash") loadTrash(true);
    else loadDirectory(state.currentDirectory, true);
  }

  function maybeLoadNextBrowserPage(scroller) {
    if (!scroller || scroller.hidden || !browserNextCursor() || browserPageLoading()) return;
    if (scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - (listRowHeight * 6)) loadNextBrowserPage();
  }

  function syncBrowserPagingSentinels() {
    const hasNextPage = Boolean(browserNextCursor());
    byID("list-page-sentinel").hidden = !hasNextPage || state.viewMode !== "list";
    byID("grid-page-sentinel").hidden = !hasNextPage || state.viewMode !== "grid";
  }

  function setupAutomaticPaging() {
    if (!("IntersectionObserver" in window)) return;
    const observe = (rootID, sentinelID, action) => {
      const root = byID(rootID);
      const sentinel = byID(sentinelID);
      const observer = new IntersectionObserver((entries) => {
        if (entries.some((entry) => entry.isIntersecting)) action();
      }, { root, rootMargin: "0px 0px 240px" });
      observer.observe(sentinel);
    };
    observe("list-presentation", "list-page-sentinel", loadNextBrowserPage);
    observe("media-grid", "grid-page-sentinel", loadNextBrowserPage);
    observe("users-table-scroll", "users-page-sentinel", loadMoreUsers);
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
      const entrySize = knownEntrySize(entry);
      return (!query || entry.name.toLocaleLowerCase().includes(query)) &&
        (!kind || entry.kind === kind) &&
        (!media || mediaCategory(entry) === media) &&
        (!Number.isFinite(minimumSize) || (entrySize !== null && entrySize >= minimumSize)) &&
        (!Number.isFinite(maximumSize) || (entrySize !== null && entrySize <= maximumSize)) &&
        (!Number.isFinite(changed) || (changed >= modifiedAfter && changed <= modifiedBefore)) &&
        (!previewState || (knownPreviewState ? knownPreviewState.state : "missing") === previewState);
    });
    const [sort, order] = byID("file-sort").value.split(":");
    const direction = order === "desc" ? -1 : 1;
    const compare = (left, right) => {
      if (sort === "modified") return direction * (new Date(left.modifiedAt).valueOf() - new Date(right.modifiedAt).valueOf());
      if (sort === "size") return compareEntrySizes(left, right, direction);
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
    const selectionAccess = ownerAccess || trashAccess;
    const selectionKey = entrySelectionKey(entry);
    tile.className = `media-tile${selectionAccess && state.selected.has(selectionKey) ? " selected" : ""}`;
    tile.setAttribute("role", "gridcell");
    tile.setAttribute("aria-rowindex", String(Math.floor(index / columns) + 1));
    tile.setAttribute("aria-colindex", String(index % columns + 1));
    if (trashAccess) tile.setAttribute("aria-label", `Trashed ${entry.name}`);
    const checkbox = document.createElement("input");
    checkbox.className = "selectable-only";
    checkbox.type = "checkbox";
    checkbox.checked = state.selected.has(selectionKey);
    checkbox.setAttribute("aria-label", `Select ${entry.name}`);
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) state.selected.set(selectionKey, entry); else state.selected.delete(selectionKey);
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
    open.append(frame, text("strong", entry.name, "media-tile-name"), text("span", formatEntrySize(entry), "media-tile-meta"));
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
    const selectionAccess = ownerAccess || trashAccess;
    const selectionKey = entrySelectionKey(entry);
    if (selectionAccess && state.selected.has(selectionKey)) row.className = "selected";
    const selectCell = document.createElement("td");
    selectCell.className = "selectable-only";
    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.checked = state.selected.has(selectionKey);
    checkbox.setAttribute("aria-label", `Select ${entry.name}`);
    checkbox.addEventListener("change", () => {
      if (checkbox.checked) state.selected.set(selectionKey, entry); else state.selected.delete(selectionKey);
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
    const sizeCell = text("td", formatEntrySize(entry));
    const detailsCell = text("td", formatEntryDetails(entry));
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
    row.append(selectCell, nameCell, sizeCell, detailsCell, dateCell, actionCell);
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
    const selectionAccess = state.browserAccess === "owner" || state.browserAccess === "trash";
    byID("selection-bar").hidden = count === 0 || !selectionAccess;
    if (count > 0) byID("app").dataset.selectionActive = "true";
    else delete byID("app").dataset.selectionActive;
    byID("selection-count").textContent = `${count} selected`;
    byID("download-selected").disabled = count !== 1 || [...state.selected.values()][0].kind !== "file";
    byID("share-selected").disabled = count !== 1;
    byID("restore-selected").disabled = count === 0;
    byID("delete-selected-permanently").disabled = count === 0;
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
