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
    if (!state.user || !state.user.userID || (state.config && state.config.localFixture)) return;
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
    const available = state.transfers.length > 0 || state.transferGroups.size > 0;
    const shouldOpen = available && open;
    if (shouldOpen && focus) state.transferSheetOpener = document.activeElement;
    panel.hidden = !shouldOpen;
    byID("transfer-scrim").hidden = !shouldOpen;
    launcher.hidden = !available;
    launcher.setAttribute("aria-expanded", String(shouldOpen));
    const modal = shouldOpen && matchMedia("(max-width: 760px)").matches;
    for (const view of byID("authenticated-view").querySelectorAll(".view")) view.inert = shouldOpen;
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
    if (!state.uploadBatchActive) {
      const batch = nextUploadAdmissionBatch();
      if (batch.length > 1) {
        state.uploadBatchActive = true;
        admitUploadBatch(batch).finally(() => {
          state.uploadBatchActive = false;
          scheduleTransferStructureRender();
          pumpTransfers();
        });
      }
    }
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
      const admitted = Boolean(transfer.pendingCapability || transfer.uploadID);
      if (transfer.state === "queued" && transfer.file instanceof File && (admitted || !state.uploadBatchActive) && (!transfer.groupID || state.transferGroups.get(transfer.groupID)?.state !== "preparing")) return transfer;
    }
    return null;
  }

  function nextUploadAdmissionBatch() {
    const batch = [];
    for (const transfer of state.transfers) {
      if (batch.length >= 100) break;
      if (transfer.state !== "queued" || !(transfer.file instanceof File) || transfer.uploadID || transfer.pendingCapability) continue;
      if (transfer.groupID && state.transferGroups.get(transfer.groupID)?.state === "preparing") continue;
      batch.push(transfer);
    }
    return batch;
  }

  function uploadInitializationRequest(transfer, includeItemKey = false) {
    const request = {
      path: transfer.directory,
      name: transfer.name,
      size: transferFileSize(transfer),
      mediaType: transferMediaType(transfer),
      conflict: "rename",
      resumable: true,
    };
    if (includeItemKey) request.idempotencyKey = transfer.id;
    return request;
  }

  function uploadBatchItemError(kind) {
    const error = new Error("Upload initialization failed.");
    error.batchErrorKind = kind || "internal";
    return error;
  }

  function failUploadPreparation(transfer, error) {
    if (!["preparing", "queued"].includes(transfer.state) || transfer.cancelRequested) return;
    if (transferFailureIsRetryable(error)) {
      scheduleTransferRetry(transfer, error);
      return;
    }
    transitionTransfer(transfer, "failed", friendlyError(error, "Upload could not be initialized."), error instanceof APIError ? error.code : error.batchErrorKind || "terminal");
    if (!transfer.groupID) announce(`${transfer.name} failed to upload.`, true);
  }

  async function admitUploadBatch(transfers) {
    for (const transfer of transfers) {
      transfer.cancelRequested = false;
      transitionTransfer(transfer, "preparing");
    }
    scheduleTransferStructureRender();
    try {
      await Promise.all(transfers.map((transfer) => ensureDirectories(transfer.baseDirectory, transfer.relativeDirectory)));
      const pending = transfers.filter((transfer) => transfer.state === "preparing" && !transfer.cancelRequested);
      if (!pending.length) return;
      const response = await api("/api/v1/uploads/batch", {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: { uploads: pending.map((transfer) => uploadInitializationRequest(transfer, true)) },
      });
      const results = response && Array.isArray(response.uploads) ? response.uploads : [];
      if (results.length !== pending.length) throw uploadBatchItemError("internal");
      for (const [offset, transfer] of pending.entries()) {
        const result = results[offset];
        if (!result || result.index !== offset || !result.capability) {
          failUploadPreparation(transfer, uploadBatchItemError(result && result.errorKind));
          continue;
        }
        transfer.uploadID = result.capability.uploadID;
        if (transfer.cancelRequested || transfer.state === "cancelled") {
          api(`/api/v1/uploads/${encodeURIComponent(transfer.uploadID)}`, { method: "DELETE", body: {} }).catch(() => {});
          continue;
        }
        transfer.pendingCapability = result.capability;
        transitionTransfer(transfer, "queued");
      }
    } catch (error) {
      for (const transfer of transfers) failUploadPreparation(transfer, error);
    }
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
    if (error && ["rate_limited", "unavailable", "internal"].includes(error.batchErrorKind)) return true;
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
      let capability = transfer.pendingCapability || null;
      transfer.pendingCapability = null;
      const mediaType = transferMediaType(transfer);
      const size = transferFileSize(transfer);
      if (!capability) {
        await ensureDirectories(transfer.baseDirectory, transfer.relativeDirectory);
        capability = await api("/api/v1/uploads", { method: "POST", headers: { "Idempotency-Key": transfer.id }, body: uploadInitializationRequest(transfer), signal: transfer.controller.signal });
      }
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

  function transferPercent(transfer) {
    const total = transferFileSize(transfer);
    const confirmed = Math.min(transfer.confirmed, total);
    return total > 0 ? Math.round(confirmed / total * 100) : transfer.state === "complete" ? 100 : 0;
  }

  function transferMetricText(transfer) {
    const total = transferFileSize(transfer);
    const confirmed = Math.min(transfer.confirmed, total);
    const percent = transferPercent(transfer);
    let eta = "Waiting";
    if (transfer.state === "uploading") eta = transfer.speedBps > 0 ? formatDuration((total - confirmed) / transfer.speedBps) : "Calculating ETA";
    else if (transfer.state === "preparing") eta = "Preparing";
    else if (transfer.state === "retry-wait") eta = transfer.nextRetryAt > Date.now() ? `Retry in ${formatDuration((transfer.nextRetryAt - Date.now()) / 1000)}` : "Retrying";
    else if (transfer.state === "paused") eta = "Offline";
    else if (transfer.state === "needs-source") eta = "Source needed";
    else if (transfer.state === "complete") eta = "Complete";
    else if (transfer.state === "cancelled") eta = "Cancelled";
    return `${percent}% · ${eta} · ${formatRate(transfer.speedBps)}`;
  }

  function transferRow(transfer) {
    const row = document.createElement("div");
    row.className = `transfer-row ${transfer.state}`;
    const label = transfer.groupID ? transfer.relativePath : transfer.name;
    const tail = document.createElement("div");
    tail.className = "transfer-row-tail";
    if (transfer.state === "needs-source") tail.append(iconButton("upload", `Reconnect upload source for ${label}`, () => openTransferReconnect(transfer.groupID || ""), "transfer-row-actions", "Reconnect source"));
    if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(transfer.state)) tail.append(iconButton("x", `Cancel upload ${label}`, () => cancelTransfer(transfer), "transfer-row-actions", "Cancel upload"));
    if ((transfer.state === "failed" || (!transfer.groupID && transfer.state === "cancelled")) && transferSourceAvailable(transfer)) {
      tail.append(iconButton("refresh", `Retry upload ${label}`, () => retryTransfer(transfer), "transfer-row-actions", "Retry upload"));
    }
    const header = document.createElement("div");
    header.className = "transfer-row-header";
    header.append(text("strong", label, "transfer-row-main"), tail);
    row.dataset.transferId = transfer.id;
    if (transfer.state === "failed") {
      const failure = document.createElement("div");
      failure.className = "transfer-row-failure";
      failure.append(
        text("span", transfer.error || "Upload failed.", "transfer-row-error"),
        text("span", `${transferPercent(transfer)}%`, "transfer-row-failure-percent"),
      );
      const progress = document.createElement("progress");
      progress.className = "transfer-row-progress transfer-row-progress-error";
      progress.max = Math.max(1, transferFileSize(transfer));
      progress.value = transfer.confirmed;
      progress.setAttribute("aria-label", `Failed upload progress for ${label}`);
      row.append(header, failure, progress);
      return row;
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
    row.append(header, metrics, progress);
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
    const header = document.createElement("div");
    header.className = "transfer-row-header";
    header.append(text("strong", group.name, "transfer-row-main"), tail);
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
    row.append(header, metrics, progress);
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
    const eta = summary.eta.replace(" remaining", "");
    byID("transfer-eta").textContent = /^\d/.test(eta) ? `ETA ${eta}` : eta === "Calculating ETA" ? "ETA …" : eta;
    byID("transfer-speed").textContent = formatRate(summary.speedBps);
    byID("transfer-volume").textContent = `${formatBytes(summary.confirmedBytes)} / ${formatBytes(summary.totalBytes)}`;
    byID("transfer-active").textContent = `${summary.active.toLocaleString()} active`;
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
    const launcherLabel = `View transfers, ${summary.percent}% complete, ${formatRate(summary.speedBps)}, ${summary.eta}, ${summary.active} active of ${summary.totalCount} files`;
    byID("open-transfers").setAttribute("aria-label", launcherLabel);
    byID("open-transfers").dataset.tooltip = launcherLabel;
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
      if (!state.expandedTransferGroups.has(group.id)) continue;
      const filteredFiles = orderedTransfers(transfers.filter((transfer) => transferMatchesFilter(transfer, filter) && transferMatchesSearch(transfer)));
      for (const transfer of filteredFiles) {
        projection.push({ kind: "transfer", transfer, nested: true });
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
        if (transfer.state === "failed") {
          const error = row.querySelector(".transfer-row-error");
          const percent = row.querySelector(".transfer-row-failure-percent");
          if (error) error.textContent = transfer.error || "Upload failed.";
          if (percent) percent.textContent = `${transferPercent(transfer)}%`;
          progress.max = Math.max(1, transferFileSize(transfer));
          progress.value = transfer.confirmed;
          continue;
        }
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
    if (!state.config || !state.config.localFixture) return;
    if (state.transfers.length > 0 || state.transferGroups.size > 0) return;

    const groupID = "local-transfer-preview";
    const baseDirectory = "/Photography";
    let totalSize = 0;
    for (let index = 0; index < 2000; index += 1) {
      // Keep the four animated rows active beyond the longest instrumented E2E
      // workflow. Small fixture files reached their ceiling before the focus-
      // preservation assertion could observe a progress-only render.
      const size = index < 4 ? (8 + index) * (2 ** 30) : (1 + (index % 24)) * (1 << 20);
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
    setTransferSheetOpen(false);

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
