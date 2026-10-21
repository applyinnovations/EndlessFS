  function transferFileSize(transfer) {
    return Number.isSafeInteger(transfer.size) ? transfer.size : transfer.file instanceof File ? transfer.file.size : 0;
  }

  // Preparation is readiness of the whole declared selection, weighted by
  // declared bytes (one unit for an empty file). A file earns its units once
  // all prerequisites are ready; published siblings are always credited. Local
  // reads and this readiness measure never advance acknowledged upload bytes.
  function preparationWeight(transfer) { return Math.max(1, transferFileSize(transfer)); }
  function preparationCredit(transfer, state = transfer.state) { return state === "complete" || transfer.preparationReady ? preparationWeight(transfer) : 0; }
  function preparationPhase(transfer, state = transfer.state) {
    return state === "complete" || transfer.preparationReady ? "" : transfer.preparationPhase || (state === "needs-source" ? "source" : "sessions");
  }
  function changePreparationSummary(summary, prior, next, priorPhase, nextPhase) {
    if (!summary) return;
    summary.preparedUnits = Math.max(0, (summary.preparedUnits || 0) + next - prior);
    summary.preparationPhases ||= {};
    if (priorPhase) summary.preparationPhases[priorPhase] = Math.max(0, (summary.preparationPhases[priorPhase] || 0) - 1);
    if (nextPhase) summary.preparationPhases[nextPhase] = (summary.preparationPhases[nextPhase] || 0) + 1;
    finalizeTransferSummary(summary);
  }
  function setPreparationState(transfer, phase, ready = false) {
    const prior = preparationCredit(transfer), priorPhase = preparationPhase(transfer);
    transfer.preparationPhase = phase;
    transfer.preparationReady = ready;
    for (const summary of [state.transferSummary, state.transferOverviewIDs.has(transfer.id) ? state.transferOverview : null, state.transferGroups.get(transfer.groupID)?.transferSummary]) {
      changePreparationSummary(summary, prior, preparationCredit(transfer), priorPhase, preparationPhase(transfer));
    }
    scheduleTransferRender();
  }
  function preparationFeedback(summary) {
    const names = { source: "Waiting for source", directories: "Preparing folders", duplicates: "Comparing files", fingerprints: "Reading source fingerprints", recovery: "Recovering upload sessions", sessions: "Preparing upload sessions" };
    return Object.entries(names).filter(([key]) => summary.preparationPhases?.[key] > 0).map(([key, label]) => `${label} (${summary.preparationPhases[key].toLocaleString()})`).join(" · ");
  }
  function preserveTransferError(value) {
    if (!value.error) return;
    value.errorHistory = [...(value.errorHistory || []), { message: String(value.error).slice(0, 240), code: String(value.errorCode || "").slice(0, 80), at: Date.now() }].slice(-3);
    value.error = "";
    value.errorCode = "";
  }
  function beginTransferAttempt(transfer, restartExpired = false) {
    transfer.directoryPreparationController?.abort();
    preserveTransferError(transfer);
    const group = state.transferGroups.get(transfer.groupID);
    if (group) {
      preserveTransferError(group);
      // An explicit retry revalidates its destination ancestry. Another tab
      // or replica may have moved/deleted a previously prepared directory.
      let path = transfer.baseDirectory;
      for (const component of (transfer.relativeDirectory || "").split("/").filter(Boolean)) {
        path = joinPath(path, component);
        group.preparedDirectories?.delete(path);
      }
      scheduleTransferGroupPersistence(group);
    }
    transfer.attemptGeneration = (transfer.attemptGeneration || 0) + 1;
    transfer.retryCount = 0;
    transfer.nextRetryAt = 0;
    transfer.allowExpiredRestart = restartExpired;
    transfer.pendingCapability = null;
    setPreparationState(transfer, transfer.uploadID ? "recovery" : "source");
    queueTransferPersistence(transfer);
  }

  const uploadLifecyclePageSize = 100;

  function uploadNamespaceMutation(path, options, prepareBody = null) {
    const owner = state.user?.userID;
    return withUploadNamespaceTurn(() => {
      if (owner !== state.user?.userID || options.signal?.aborted) throw new DOMException("Upload request suspended", "AbortError");
      if (prepareBody) {
        const body = prepareBody();
        if (!body) return null;
        return api(path, { ...options, body });
      }
      return api(path, options);
    });
  }
  let uploadLifecycleQueue = [];
  let uploadLifecycleActive = false;
  let activeUploadLifecyclePage = null;
  let uploadLifecycleRetryAt = 0;
  let uploadLifecycleRetryTimer = 0;
  let uploadAdmissionRetryAt = 0;
  let uploadAdmissionRetryTimer = 0;
  function uploadLifecycle(transfer, resume = false) {
    const uploadID = transfer.uploadID;
    const generation = transfer.attemptGeneration || 0;
    const owner = state.user?.userID;
    const signal = transfer.controller?.signal;
    return new Promise((resolve, reject) => {
      const job = { transfer, uploadID, generation, owner, resume, signal, resolve, reject, settled: false, page: null, capacityLimit: uploadLifecyclePageSize };
      job.finish = (error, item) => {
        if (job.settled) return;
        job.settled = true;
        signal?.removeEventListener("abort", job.abort);
        if (error) reject(error); else resolve(item);
      };
      job.abort = () => {
        job.finish(new DOMException("Upload recovery cancelled", "AbortError"));
        if (job.page?.jobs.every((candidate) => candidate.settled)) job.page.controller.abort();
      };
      if (signal?.aborted) { job.abort(); return; }
      signal?.addEventListener("abort", job.abort, { once: true });
      uploadLifecycleQueue.push(job);
      queueMicrotask(flushUploadLifecycle);
    });
  }
  async function flushUploadLifecycle() {
    if (uploadLifecycleActive) return;
    uploadLifecycleQueue = uploadLifecycleQueue.filter((job) => {
      if (!job.settled && (job.transfer.cleared || job.owner !== state.user?.userID || job.uploadID !== job.transfer.uploadID || job.generation !== (job.transfer.attemptGeneration || 0))) job.abort();
      return !job.settled;
    });
    if (!uploadLifecycleQueue.length) return;
    if (Date.now() < uploadLifecycleRetryAt) {
      if (!uploadLifecycleRetryTimer) uploadLifecycleRetryTimer = window.setTimeout(() => { uploadLifecycleRetryTimer = 0; flushUploadLifecycle(); }, uploadLifecycleRetryAt - Date.now());
      return;
    }
    uploadLifecycleActive = true;
    const resume = uploadLifecycleQueue[0].resume;
    const capacityLimit = uploadLifecycleQueue[0].capacityLimit;
    const jobs = uploadLifecycleQueue.filter((job) => job.resume === resume && job.capacityLimit === capacityLimit).slice(0, capacityLimit);
    const selected = new Set(jobs);
    uploadLifecycleQueue = uploadLifecycleQueue.filter((job) => !selected.has(job));
    const page = { jobs, controller: new AbortController() };
    activeUploadLifecyclePage = page;
    for (const job of jobs) job.page = page;
    try {
      const response = await api(`/api/v1/uploads/batch/${resume ? "resume" : "status"}`, { method: "POST", body: { uploadIDs: jobs.map((job) => job.uploadID) }, signal: page.controller.signal });
      if (!Array.isArray(response?.uploads) || response.uploads.length !== jobs.length) throw new TypeError("Upload recovery returned an incomplete page.");
      if (response.uploads.some((item) => item.errorKind === "rate_limited")) uploadLifecycleRetryAt = Date.now() + transferRetryBaseDelay;
      for (const [index, job] of jobs.entries()) {
        if (job.settled) continue;
        if (job.transfer.cleared || job.owner !== state.user?.userID || job.uploadID !== job.transfer.uploadID || job.generation !== (job.transfer.attemptGeneration || 0)) { job.abort(); continue; }
        const item = response.uploads[index];
        if (item?.index !== index || item.uploadID !== job.uploadID || item.status && item.status.uploadID !== job.uploadID || item.capability && item.capability.uploadID !== job.uploadID) { job.finish(new TypeError("Upload recovery returned a misbound item.")); continue; }
        if (item.capacityDeferred) {
          if (jobs.length < 2 || item.nextAction !== "smaller_page" || item.errorKind || item.capability || !item.status || item.status.uploadID !== job.uploadID) {
            job.finish(new TypeError("Upload recovery returned an invalid capacity deferral.")); continue;
          }
          // This is response partitioning, not a failed transfer attempt.
          // Keep the exact durable upload and promise while halving its next
          // cohort; even a page containing only deferrals terminates at one.
          job.capacityLimit = Math.max(1, Math.floor(jobs.length / 2));
          job.page = null;
          uploadLifecycleQueue.push(job);
          continue;
        }
        job.finish(null, item);
      }
    } catch (error) {
      if (error instanceof APIError && (error.status === 429 || error.status >= 500) || error instanceof TypeError) uploadLifecycleRetryAt = Date.now() + Math.max(error.retryAfterMs || 0, transferRetryBaseDelay + Math.floor(Math.random() * transferRetryBaseDelay));
      for (const job of jobs) job.finish(error);
    } finally {
      if (activeUploadLifecyclePage === page) activeUploadLifecyclePage = null;
      uploadLifecycleActive = false;
      queueMicrotask(flushUploadLifecycle);
    }
  }

  function uploadLifecycleError(item) {
    const status = ({ invalid: 400, unauthenticated: 401, unauthorized: 403, not_found: 404, conflict: 409, precondition_failed: 412, rate_limited: 429, unavailable: 503 })[item.errorKind] || 500;
    const error = new APIError(new Response(null, { status }), { title: "Upload recovery needs attention", code: item.errorKind, requestID: item.requestID });
    error.batchErrorKind = item.errorKind || "internal";
    if (typeof item.retryable === "boolean") error.batchRetryable = item.retryable;
    return error;
  }

  async function readUploadStatus(transfer) {
    const item = await uploadLifecycle(transfer);
    if (item.errorKind) throw uploadLifecycleError(item);
    if (!item.status) throw new TypeError("Upload recovery omitted status.");
    return item.status;
  }

  function restoreTransferProgress(transfer, confirmed) {
    const priorConfirmed = transfer.confirmed, priorSpeed = transfer.speedBps;
    transfer.confirmed = Math.max(0, Math.min(Number(confirmed) || 0, transferFileSize(transfer)));
    transfer.speedBps = 0;
    for (const summary of [state.transferSummary, state.transferOverviewIDs.has(transfer.id) ? state.transferOverview : null, state.transferGroups.get(transfer.groupID)?.transferSummary]) updateAggregateProgress(summary, transfer, priorConfirmed, priorSpeed);
    queueTransferPersistence(transfer);
  }

  // File tasks are cheap; concurrent native file streams are not. Materialize
  // bounded chunks through one reader and retain each reservation until its
  // provider request settles. Slow sources then apply backpressure to reads,
  // without occupying a browser network stream or loading a whole file.
  const uploadSourceChunkBytes = 8 << 20;
  let uploadSourceBudget = 4 * uploadSourceChunkBytes;
  const uploadSource = { usedBytes: 0, peakBytes: 0, readers: 0, peakReaders: 0, queue: [], reading: false };
  const uploadRates = new Map();
  const uploadEstimates = new Map();
  let uploadProgressTimer = 0;
  let uploadMeasurementOffset = 0;
  let lastUploadAcknowledgementAt = 0;
  function uploadMeasurementNow() { return performance.now() + uploadMeasurementOffset; }

  function uploadCohort(transfer) { return transfer.groupID || transfer.id; }

  function measuredUploadRate(cohorts, now = uploadMeasurementNow(), minimumBytes = uploadSourceChunkBytes) {
    let rate = 0;
    for (const cohort of cohorts) {
      const measure = uploadRates.get(cohort);
      if (!measure) continue;
      const start = Math.max(measure.startedAt, now - 15000);
      measure.samples = measure.samples.filter((sample) => sample.at > start);
      const bytes = measure.samples.reduce((sum, sample) => sum + sample.bytes, 0);
      const raw = now > start ? bytes * 1000 / (now - start) : 0;
      if (now - measure.startedAt < 3000 || measure.samples.length < 2 || bytes < minimumBytes) {
        measure.rate = 0;
        measure.rateAt = 0;
        continue;
      }
      if (!measure.rateAt || now - measure.rateAt >= 1000) {
        const elapsed = measure.rateAt ? now - measure.rateAt : 15000;
        const weight = 1 - Math.exp(-elapsed / 15000);
        measure.rate = raw === 0 ? 0 : measure.rate ? measure.rate + weight * (raw - measure.rate) : raw;
        measure.rateAt = now;
      }
      rate += measure.rate || 0;
    }
    return rate;
  }

  function uploadCheckBytes(transfer) {
    const size = transferFileSize(transfer);
    return !transfer.planPhase || transfer.planPhase === "done" || transfer.md5 && transfer.crc32c
      ? size : Math.min(size, transfer.checkedBytes || 0);
  }

  function recordUploadCheckProgress(transfer, bytes) {
    const prior = uploadCheckBytes(transfer);
    transfer.checkedBytes = Math.max(transfer.checkedBytes || 0, Math.min(bytes, transferFileSize(transfer)));
    const delta = uploadCheckBytes(transfer) - prior;
    for (const summary of [state.transferSummary, state.transferOverviewIDs.has(transfer.id) ? state.transferOverview : null, state.transferGroups.get(transfer.groupID)?.transferSummary]) {
      if (summary) summary.checkedBytes = Math.min(summary.totalBytes, (summary.checkedBytes || 0) + delta);
    }
    scheduleTransferRender();
  }

  function refreshUploadRates() {
    const active = state.transfers.filter((transfer) => transfer.state === "uploading" && !transfer.fixture);
    if (!active.length) return;
    const now = uploadMeasurementNow();
    const summaryRate = (summary, transfers) => {
      if (!summary) return;
      summary.speedBps = measuredUploadRate(new Set(transfers.map(uploadCohort)), now, Math.min(summary.remainingBytes || summary.totalBytes, uploadSourceChunkBytes));
      finalizeTransferSummary(summary);
    };
    summaryRate(state.transferSummary, active);
    summaryRate(state.transferOverview, active.filter((transfer) => state.transferOverviewIDs.has(transfer.id)));
    for (const group of state.transferGroups.values()) if (group.transferSummary?.counts.uploading) summaryRate(group.transferSummary, active.filter((transfer) => transfer.groupID === group.id));
  }

  function drainUploadSource() {
    if (uploadSource.reading) return;
    const next = uploadSource.queue[0];
    if (!next || uploadSource.paused || uploadSource.usedBytes + next.bytes > uploadSourceBudget && uploadSource.usedBytes !== 0) return;
    uploadSource.queue.shift();
    next.signal.removeEventListener("abort", next.abort);
    if (next.signal.aborted) { next.reject(new DOMException("Upload cancelled", "AbortError")); drainUploadSource(); return; }
    const reservation = Math.min(next.bytes, uploadSourceBudget);
    uploadSource.usedBytes += reservation;
    uploadSource.peakBytes = Math.max(uploadSource.peakBytes, uploadSource.usedBytes);
    let released = false;
    const release = () => {
      if (released) return;
      released = true;
      uploadSource.usedBytes -= reservation;
      drainUploadSource();
    };
    // A provider's whole-object framing can exceed the chunk budget. Preserve
    // its native streaming contract exclusively instead of buffering the file.
    if (next.bytes > uploadSourceBudget) { next.transfer.sourcePhase = ""; next.resolve({ body: next.blob, release }); drainUploadSource(); return; }
    uploadSource.reading = true;
    uploadSource.readers++;
    uploadSource.peakReaders = Math.max(uploadSource.peakReaders, uploadSource.readers);
    const reader = new FileReader();
    const abort = () => reader.abort();
    next.signal.addEventListener("abort", abort, { once: true });
    const finish = (error) => {
      next.signal.removeEventListener("abort", abort);
      uploadSource.reading = false;
      uploadSource.readers--;
      next.transfer.sourcePhase = "";
      if (error) { release(); next.reject(error); }
      else next.resolve({ body: reader.result, release });
      drainUploadSource();
      scheduleTransferRender();
    };
    next.transfer.sourcePhase = "reading";
    scheduleTransferRender();
    reader.onload = () => finish(null);
    reader.onabort = () => finish(new DOMException("Upload cancelled", "AbortError"));
    reader.onerror = () => {
      const error = new Error("The source could not be read. Reconnect the drive or select the file again.");
      error.name = "SourceReadError";
      finish(error);
    };
    try { reader.readAsArrayBuffer(next.blob); }
    catch { reader.onerror(); }
  }

  function readUploadSource(transfer, start, end) {
    const signal = transfer.controller.signal;
    return new Promise((resolve, reject) => {
      if (signal.aborted) { reject(new DOMException("Upload cancelled", "AbortError")); return; }
      const item = { transfer, signal, blob: transfer.file.slice(start, end), bytes: end - start, resolve, reject };
      item.abort = () => {
        const index = uploadSource.queue.indexOf(item);
        if (index >= 0) uploadSource.queue.splice(index, 1);
        transfer.sourcePhase = "";
        reject(new DOMException("Upload cancelled", "AbortError"));
        drainUploadSource();
      };
      signal.addEventListener("abort", item.abort, { once: true });
      transfer.sourcePhase = "waiting-source";
      uploadSource.queue.push(item);
      drainUploadSource();
    });
  }

  function transferSourceAvailable(transfer) {
    return transfer.file instanceof File || transfer.fixture === true;
  }

  function transferCanRetry(transfer) {
    return transferSourceAvailable(transfer) || Boolean(transfer.awaitingCompletion && transfer.uploadID && transfer.crc32c) || Boolean(transfer.pendingAction === "cancel" && transfer.uploadID);
  }

  function transferNeedsSource(transfer) {
    return transfer.state === "needs-source" || (transfer.state === "failed" && !transferCanRetry(transfer));
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

  function transferLedgerItem(transfer, ownerID = state.transferLedgerOwner) {
    return {
      key: transferLedgerKey(ownerID, transfer.id), ownerID, id: transfer.id,
      groupID: transfer.groupID || "", name: transfer.name, directory: transfer.directory,
      baseDirectory: transfer.baseDirectory, relativeDirectory: transfer.relativeDirectory || "",
      relativePath: transfer.relativePath, size: transferFileSize(transfer), mediaType: transferMediaType(transfer),
      lastModified: Number.isSafeInteger(transfer.lastModified) ? transfer.lastModified : 0,
      state: transfer.state, confirmed: Math.max(0, transfer.confirmed || 0), uploadID: transfer.uploadID || "",
	  batchID: transfer.batchID || "", batchIndex: Number.isSafeInteger(transfer.batchIndex) ? transfer.batchIndex : 0,
	  batchCount: Number.isSafeInteger(transfer.batchCount) ? transfer.batchCount : 0,
	  admissionBatchKey: transfer.admissionBatchKey || "",
      admissionProtocol: transfer.admissionProtocol || "",
      admissionIntent: transfer.admissionIntent ? { path: transfer.admissionIntent.path, name: transfer.admissionIntent.name, size: transfer.admissionIntent.size, mediaType: transfer.admissionIntent.mediaType, conflict: transfer.admissionIntent.conflict, resumable: transfer.admissionIntent.resumable, expectedVersion: transfer.admissionIntent.expectedVersion, idempotencyKey: transfer.admissionIntent.idempotencyKey } : null,
      durableAdmissionRequired: Boolean(transfer.durableAdmissionRequired),
	  admissionBatchIndex: Number.isSafeInteger(transfer.admissionBatchIndex) ? transfer.admissionBatchIndex : 0,
	  admissionBatchCount: Number.isSafeInteger(transfer.admissionBatchCount) ? transfer.admissionBatchCount : 0,
	  awaitingCompletion: Boolean(transfer.awaitingCompletion), completionBatchKey: transfer.completionBatchKey || "",
	  completionBatchIndex: Number.isSafeInteger(transfer.completionBatchIndex) ? transfer.completionBatchIndex : -1,
      retryCount: Math.max(0, transfer.retryCount || 0), nextRetryAt: Math.max(0, transfer.nextRetryAt || 0),
      errorCode: String(transfer.errorCode || "").slice(0, 80), error: String(transfer.error || "").slice(0, 240),
      errorHistory: (transfer.errorHistory || []).slice(-3),
      pendingAction: transfer.pendingAction || "", completionProtocol: transfer.completionProtocol || "",
      strategy: transfer.strategy || "keep-both", planPhase: transfer.planPhase || "done",
      md5: transfer.md5 || "", crc32c: transfer.crc32c || "", planOutcome: transfer.planOutcome || "",
      sourceExpectedMD5: transfer.sourceExpectedMD5 || "", sourceExpectedCRC32C: transfer.sourceExpectedCRC32C || "",
      targetExists: Boolean(transfer.targetExists), targetKind: transfer.targetKind || "",
      targetVersion: transfer.targetVersion || "", uploadConflict: transfer.uploadConflict || "rename",
      createdAt: transfer.createdAt || Date.now(), updatedAt: Date.now(),
    };
  }

  function transferLedgerGroup(group, ownerID = state.transferLedgerOwner) {
    return {
      key: transferLedgerKey(ownerID, group.id), ownerID, id: group.id, name: group.name,
      baseDirectory: group.baseDirectory, directories: [...(group.directories || [])], transferIDs: [...group.transferIDs],
      totalSize: group.totalSize || 0, state: group.state, error: String(group.error || "").slice(0, 240),
      errorHistory: (group.errorHistory || []).slice(-3),
      cancelled: Boolean(group.cancelled), discoveryDone: group.discoveryDone !== false,
      strategy: group.strategy || "keep-both",
      createdAt: group.createdAt || Date.now(), updatedAt: Date.now(),
    };
  }

  async function persistTransferItem(transfer) {
    if (transfer.cleared || transfer.fixture || transfer.identityNeedsPersistence || !state.transferLedgerOwner) return;
    const ownerID = state.transferLedgerOwner;
    try {
      const database = await openTransferLedger();
      if (!database || transfer.cleared || state.transferLedgerOwner !== ownerID) return;
      const transaction = database.transaction("items", "readwrite");
      transaction.objectStore("items").put(transferLedgerItem(transfer, ownerID));
      await ledgerTransactionDone(transaction);
    } catch {
      warnTransferLedger("Transfer history could not be saved on this device.");
    }
  }

	async function persistTransferItems(transfers, identityBarrier = false) {
	  if (state.config?.localFixture) return true;
	  const durable = transfers.filter((transfer) => !transfer.cleared && !transfer.fixture && (identityBarrier || !transfer.identityNeedsPersistence));
	  if (!durable.length || !state.transferLedgerOwner) return false;
	  const ownerID = state.transferLedgerOwner;
	  let transaction;
	  try {
		const database = await openTransferLedger();
		if (!database || state.transferLedgerOwner !== ownerID) return false;
		transaction = database.transaction(["items", "groups", "sources"], "readwrite");
		const store = transaction.objectStore("items");
        const written = new Map();
		for (const transfer of durable) if (!transfer.cleared) {
          store.put(transferLedgerItem(transfer, ownerID));
          written.set(transfer,transfer.id);
          if (identityBarrier) for (const priorID of transfer.replacedIdentityIDs || []) {
            const key = transferLedgerKey(ownerID,priorID);
            store.delete(key); transaction.objectStore("sources").delete(key);
          }
        }
        for (const groupID of new Set(durable.map((transfer) => transfer.groupID).filter(Boolean))) {
          const group = state.transferGroups.get(groupID);
          if (group && !group.cleared && !group.fixture) {
            const record = transferLedgerGroup(group, ownerID);
            // Other recovery pages may still be persisting members of this
            // group. Retain their last durable IDs until their own transaction.
            record.transferIDs = record.transferIDs.map((id) => {
              const transfer = state.transferByID.get(id);
              return transfer?.identityNeedsPersistence && !written.has(transfer) ? transfer.replacedIdentityIDs?.[0] || id : id;
            });
            transaction.objectStore("groups").put(record);
          }
        }
		await ledgerTransactionDone(transaction);
        if (identityBarrier) for (const [transfer,id] of written) if (transfer.id === id) {
          transfer.identityNeedsPersistence = false; transfer.replacedIdentityIDs = [];
        }
        return true;
	  } catch {
		try { transaction?.abort(); } catch { /* already aborted or settled */ }
		warnTransferLedger("Transfer history could not be saved on this device.");
        return false;
	  }
	}

  async function persistTransferGroup(group) {
    if (group.cleared || group.fixture || !state.transferLedgerOwner) return;
    if (group.transferIDs.some((id) => state.transferByID.get(id)?.identityNeedsPersistence)) return;
    const ownerID = state.transferLedgerOwner;
    try {
      const database = await openTransferLedger();
      if (!database || group.cleared || state.transferLedgerOwner !== ownerID) return;
      const transaction = database.transaction("groups", "readwrite");
      transaction.objectStore("groups").put(transferLedgerGroup(group, ownerID));
      await ledgerTransactionDone(transaction);
    } catch {
      warnTransferLedger("Transfer history could not be saved on this device.");
    }
  }

  const transferGroupPersistQueue = new Map();
  let transferGroupPersistTimer = 0;
  function scheduleTransferGroupPersistence(group) {
    if (group.cleared || group.fixture || !state.transferLedgerOwner) return;
    const owner = state.transferLedgerOwner;
    transferGroupPersistQueue.set(`${owner}:${group.id}`, { owner, group });
    if (transferGroupPersistTimer) return;
    transferGroupPersistTimer = window.setTimeout(async () => {
      transferGroupPersistTimer = 0;
      const pending = [...transferGroupPersistQueue.values()];
      transferGroupPersistQueue.clear();
      await Promise.all(pending.filter((item) => item.owner === state.transferLedgerOwner).map((item) => persistTransferGroup(item.group)));
    }, 500);
  }

  function queueTransferPersistence(transfer) {
    if (transfer.cleared || transfer.fixture || !state.transferLedgerOwner) return;
    state.transferPersistQueue.set(transfer.id, transfer);
    if (state.transferPersistTimer) return;
    state.transferPersistTimer = window.setTimeout(async () => {
      state.transferPersistTimer = 0;
      const pending = [...state.transferPersistQueue.values()];
      state.transferPersistQueue.clear();
      await persistTransferItems(pending);
    }, 500);
  }

  async function persistTransferSource(transfer, handle) {
    if (transfer.cleared || !handle || transfer.fixture || !state.transferLedgerOwner) return;
    const ownerID = state.transferLedgerOwner;
    try {
      const database = await openTransferLedger();
      if (!database || transfer.cleared || state.transferLedgerOwner !== ownerID) return;
      const transaction = database.transaction("sources", "readwrite");
      transaction.objectStore("sources").put({ key: transferLedgerKey(ownerID, transfer.id), ownerID, handle });
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
      resetUploadPlanForReconnectedSource(transfer);
      return true;
    } catch {
      return false;
    }
  }

  function resetUploadPlanForReconnectedSource(transfer) {
    if (["complete", "cancelled"].includes(transfer.state)) return;
    if (transfer.uploadID) {
      transfer.sourceExpectedMD5 ||= transfer.md5 || "";
      transfer.sourceExpectedCRC32C ||= transfer.crc32c || "";
    }
    // A newly acquired File object is not cryptographically identical merely
    // because its name, size, and modification time match. Never apply a
    // fingerprint persisted for a prior local source instance.
    transfer.planPhase = transfer.strategy === "keep-both" || transfer.uploadID ? "done" : "size-pending";
    transfer.planOutcome = "";
    transfer.md5 = "";
    transfer.crc32c = "";
    transfer.checkedBytes = 0;
    transfer.targetExists = false;
    transfer.targetKind = "";
    transfer.targetVersion = "";
    transfer.uploadConflict = "rename";
  }

  async function reconcileRestoredTransfer(transfer) {
    // Terminal failures require an explicit retry or clear action, including
    // failed finalization with an available source or a retained checksum.
    if (["complete", "cancelled", "failed"].includes(transfer.state)) return;
	// A permission-granted stored handle has satisfied the old source request.
	// Clear that obsolete code before the first smart-planning response; keep
	// useful bounded history and leave unrelated failures for their own checks.
	if (transfer.file instanceof File && transfer.errorCode === "source_required") {
	  preserveTransferError(transfer);
	  transfer.error = "";
	  transfer.errorCode = "";
	}
	if (transfer.awaitingCompletion && transfer.uploadID && transfer.crc32c) {
	  transfer.state = "uploading";
	  transfer.completionReadyAt = uploadMeasurementNow();
	  transfer.error = "";
	} else if (!(transfer.file instanceof File)) {
      transfer.state = "needs-source";
      transfer.errorCode = "source_required";
      transfer.error = "Reconnect the source to continue.";
    } else if (transfer.planPhase && transfer.planPhase !== "done") {
      transfer.state = navigator.onLine ? "preparing" : "paused";
      transfer.error = navigator.onLine ? "Resuming duplicate check." : "Waiting for a network connection.";
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

  async function reconcileUploadAdmissionConflict(transfer, error) {
    if (!(error instanceof APIError) || error.status !== 409 || !transfer.uploadID) return false;
    try {
      const status = await readUploadStatus(transfer);
      transfer.confirmed = Math.max(transfer.confirmed, Number(status.confirmedOffset) || 0);
      if (status.state === "completed") {
        recordTransferProgress(transfer, transferFileSize(transfer));
        transfer.retryCount = 0;
        transfer.nextRetryAt = 0;
        transitionTransfer(transfer, "complete");
        queueUploadDirectoryRefresh(transfer.directory);
        return true;
      }
      if (status.state === "aborted" || status.state === "expired") {
        transitionTransfer(transfer, "failed", status.state === "expired" ? "Upload expired. Reconnect the source to start again." : "Upload was cancelled.", status.state);
        return true;
      }
    } catch {
      // The original admission error remains the useful failure when the
      // narrowly scoped terminal-state recovery lookup is itself unavailable.
    }
    return false;
  }

  async function restoreTransferLedger() {
    if (!state.user || !state.user.userID || (state.config && state.config.localFixture)) return;
    state.transferLedgerOwner = state.user.userID;
    const ownerID = state.transferLedgerOwner;
    try {
      const [itemRecords, groupRecords, storedSources] = await Promise.all([
        transferLedgerRecords("items", state.transferLedgerOwner),
        transferLedgerRecords("groups", state.transferLedgerOwner),
        transferLedgerRecords("sources", state.transferLedgerOwner),
      ]);
      if (state.transferLedgerOwner !== ownerID || state.user?.userID !== ownerID) return;
      const sourceRecords = new Map(storedSources.map((source) => [source.key.slice(source.key.indexOf(":") + 1), source]));
      // The workspace remains interactive while history loads. Preserve any
      // sources and live workers queued during the asynchronous ledger read.
      const restored = itemRecords.filter((record) => !state.transferByID.has(record.id)).map((record) => ({
        ...record, file: null, controller: null, speedBps: 0, lastProgressAt: 0,
        preparationReady: Boolean(record.awaitingCompletion && record.uploadID && record.crc32c), attemptGeneration: 0,
        lastProgressBytes: record.confirmed || 0, recoveryFailures: 0,
      }));
      state.transfers = [...restored, ...state.transfers];
      state.transferByID = new Map(state.transfers.map((transfer) => [transfer.id, transfer]));
      state.transferGroups = new Map([...groupRecords.map((record) => [record.id, {
        ...record, preparedDirectories: new Set(), refreshed: record.state === "complete", failureAnnounced: record.state === "failed",
      }]), ...state.transferGroups]);
      let nextIndex = 0;
      const workers = Array.from({ length: Math.min(4, restored.length) }, async () => {
        while (nextIndex < restored.length) {
          const transfer = restored[nextIndex];
          nextIndex += 1;
          await restoreTransferSource(transfer, sourceRecords);
          await reconcileRestoredTransfer(transfer);
          if (state.transferLedgerOwner !== ownerID) return;
          queueTransferPersistence(transfer);
        }
      });
      await Promise.all(workers);
      if (state.transferLedgerOwner !== ownerID) return;
      for (const group of state.transferGroups.values()) updateTransferGroup(group.id, false);
      if (state.transfers.length) {
        renderTransfers();
        if (restored.length === state.transfers.length) setTransferSheetOpen(false);
        resumeUploadPlans();
        pumpTransfers();
      }
    } catch {
      warnTransferLedger("Transfer history could not be restored on this device.");
    }
  }

  async function deleteTransferLedgerEntries(itemIDs, groupIDs, ownerID = state.transferLedgerOwner) {
    if (!ownerID) return;
    itemIDs = itemIDs.flatMap((id) => [id,...(state.transferByID.get(id)?.replacedIdentityIDs || [])]);
    const database = await openTransferLedger();
    if (!database) return;
    const transaction = database.transaction(["items", "groups", "sources"], "readwrite");
    for (const id of itemIDs) {
      const key = transferLedgerKey(ownerID, id);
      transaction.objectStore("items").delete(key);
      transaction.objectStore("sources").delete(key);
    }
    for (const id of groupIDs) transaction.objectStore("groups").delete(transferLedgerKey(ownerID, id));
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
    const strategy = options.strategy || "keep-both";
    const queued = [];
    let group = groupID ? state.transferGroups.get(groupID) : null;
    if (groupID && !group) {
      group = {
        id: groupID, name: options.groupName || `${inputs.length} files`, baseDirectory, directories: [], transferIDs: [],
        totalSize: 0, state: "queued", error: "", refreshed: false, cancelled: false, failureAnnounced: false,
        discoveryDone: options.discoveryDone !== false, strategy, createdAt: Date.now(), preparedDirectories: new Set(),
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
        relativePath: input.relativePath, groupID, state: strategy === "keep-both" ? "queued" : "preparing", confirmed: 0, error: "", controller: null, uploadID: "",
        size: input.file.size, mediaType: uploadMediaType(input.file, name), lastModified: input.file.lastModified,
        strategy, planPhase: strategy === "keep-both" ? "done" : "size-pending", md5: "", crc32c: "", planOutcome: "",
        targetExists: false, targetKind: "", targetVersion: "", uploadConflict: "rename",
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
      group.strategy = strategy;
      group.state = group.transferIDs.length ? (strategy === "keep-both" ? "queued" : "preparing") : "preparing";
      await persistTransferGroup(group);
      if (!group.transferIDs.length && group.discoveryDone) prepareTransferGroup(group);
    }
    setTransferSheetOpen(true);
    rebuildTransferProjection();
    renderTransfers();
    if (strategy !== "keep-both" && options.discoveryDone !== false) planUploadTransfers(queued, group);
    pumpTransfers();
    return groupID;
  }

  async function queueFolderFiles(files, strategy = "keep-both") {
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
    for (const [groupName, group] of groups) await queueFiles(group.files, { groupName, directories: [...group.directories], strategy });
    if (looseFiles.length) await queueFiles(looseFiles, { strategy });
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

  async function discoverTransferGroup(name, discover, strategy = "keep-both") {
    const groupID = idempotencyKey();
    const files = [];
    const directories = [];
    const flush = async () => {
      if (!files.length && !directories.length) return;
      const pendingFiles = files.splice(0, files.length);
      const pendingDirectories = directories.splice(0, directories.length);
      await queueFiles(pendingFiles, { groupID, groupName: name, directories: pendingDirectories, discoveryDone: false, strategy });
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
      else if (strategy === "keep-both") updateTransferGroup(groupID, false);
      else planUploadTransfers(group.transferIDs.map((id) => state.transferByID.get(id)).filter(Boolean), group);
      await persistTransferGroup(group);
      rebuildTransferProjection();
      renderTransfers();
      pumpTransfers();
    }
  }

  function captureDroppedItems(dataTransfer) {
    const items = Array.from(dataTransfer.items || []).filter((item) => item.kind === "file");
    const files = Array.from(dataTransfer.files || []);
    const captured = items.map((item) => {
      let handleResult = Promise.resolve({ handle: null });
      if (typeof item.getAsFileSystemHandle === "function") {
        try {
          handleResult = Promise.resolve(item.getAsFileSystemHandle()).then(
            (handle) => ({ handle }), (error) => ({ handle: null, error }),
          );
        } catch (error) { handleResult = Promise.resolve({ handle: null, error }); }
      }
      const getEntry = typeof item.getAsEntry === "function" ? item.getAsEntry.bind(item) : typeof item.webkitGetAsEntry === "function" ? item.webkitGetAsEntry.bind(item) : null;
      return { handleResult, entry: getEntry ? getEntry() : null, file: typeof item.getAsFile === "function" ? item.getAsFile() : null };
    });
    return { items: captured, files };
  }

  async function queueDroppedItems(sources, strategy = "keep-both") {
    if (!sources.items.length) {
      if (!sources.files.length) throw new Error("Dropped files could not be read. Choose Upload files or Upload folder.");
      await queueFiles(sources.files, { strategy });
      return;
    }
    const looseFiles = [];
    let usedEntryAPI = false;
    for (const item of sources.items) {
      const { handle, error } = await item.handleResult;
      if (handle) {
        usedEntryAPI = true;
        if (handle.kind === "directory") {
          await discoverTransferGroup(handle.name, (onFile, onDirectory) => discoverFileSystemHandle(handle, "", onFile, onDirectory), strategy);
        } else {
          await discoverFileSystemHandle(handle, "", async (input) => { looseFiles.push(input); }, async () => {});
        }
        continue;
      }
      const legacyEntry = item.entry;
      if (legacyEntry) {
        usedEntryAPI = true;
        if (legacyEntry.isDirectory) {
          await discoverTransferGroup(legacyEntry.name, (onFile, onDirectory) => discoverLegacyEntry(legacyEntry, "", onFile, onDirectory), strategy);
        } else {
          await discoverLegacyEntry(legacyEntry, "", async (input) => { looseFiles.push(input); }, async () => {});
        }
        continue;
      }
      if (error && !item.file) throw error;
      const file = item.file;
      if (file) looseFiles.push({ file, relativePath: file.name });
    }
    if (looseFiles.length) await queueFiles(looseFiles, { strategy });
    if (!usedEntryAPI && !looseFiles.length) {
      if (!sources.files.length) throw new Error("Dropped files could not be read. Choose Upload files or Upload folder.");
      await queueFiles(sources.files, { strategy });
    }
  }

  let directoryPreparationQueue = [];
  let directoryPreparationScheduled = false;
  const directoryPreparationActive = new Set();
  const directoryPreparationEntries = new Map();

  function directoryPreparationWait(promise, signal) {
    if (!signal) return promise;
    return new Promise((resolve, reject) => {
      const abort = () => reject(new DOMException("Folder preparation cancelled", "AbortError"));
      if (signal.aborted) { abort(); return; }
      signal.addEventListener("abort", abort, { once: true });
      promise.then(resolve, reject).finally(() => signal.removeEventListener("abort", abort));
    });
  }

  function directoryPreparationNeeded(entry) {
    return entry.owner === state.user?.userID && [...entry.signals].some((signal) => !signal?.aborted);
  }

  function ensureDirectory(path, owner = state.user?.userID, signal = null) {
    const key = `${owner}:${path}`;
    if (owner !== state.user?.userID || signal?.aborted) return Promise.reject(new DOMException("Upload preparation changed", "AbortError"));
    let entry = directoryPreparationEntries.get(key);
    if (!entry || entry.page?.controller.signal.aborted) {
      let resolve, reject;
      const promise = new Promise((success, failure) => { resolve = success; reject = failure; });
      entry = { owner, path, key, promise, resolve, reject, signals: new Set(), cleanup: [] };
      state.directoryPromises.set(key, promise);
      directoryPreparationEntries.set(key, entry);
      directoryPreparationQueue.push(entry);
      if (!directoryPreparationScheduled) {
        directoryPreparationScheduled = true;
        queueMicrotask(flushDirectoryPreparation);
      }
    }
    if (!entry.signals.has(signal)) {
      entry.signals.add(signal);
      if (signal) {
        const abort = () => { if (entry.page?.entries.every((item) => !directoryPreparationNeeded(item))) entry.page.controller.abort(); };
        signal.addEventListener("abort", abort, { once: true });
        entry.cleanup.push(() => signal.removeEventListener("abort", abort));
      }
    }
    return directoryPreparationWait(entry.promise, signal);
  }

  async function flushDirectoryPreparation() {
    directoryPreparationScheduled = false;
    const entries = directoryPreparationQueue.splice(0).sort((left, right) => left.path.split("/").length - right.path.split("/").length || left.path.localeCompare(right.path));
    const settle = (entry, error = null) => {
      if (state.directoryPromises.get(entry.key) === entry.promise) state.directoryPromises.delete(entry.key);
      if (directoryPreparationEntries.get(entry.key) === entry) directoryPreparationEntries.delete(entry.key);
      for (const cleanup of entry.cleanup) cleanup();
      if (error) entry.reject(error); else entry.resolve();
    };
    let pages;
    try { pages = [...uploadControlBatches(entries, (entry) => entry.path, "paths", {}, 100)]; }
    catch (error) { for (const entry of entries) settle(entry, error); return; }
    await Promise.all(pages.map(async (page) => {
      let release;
      let slot;
      const controller = new AbortController();
      uploadActionControllers.add(controller);
      try {
        const flight = { entries: page, controller };
        for (const entry of page) entry.page = flight;
        if (page.every((entry) => !directoryPreparationNeeded(entry))) controller.abort();
        const members = new Set(page.map((entry) => entry.promise));
        const dependencies = new Set();
        for (const entry of page) {
          const parts = entry.path.split("/");
          for (let end = 2; end < parts.length; end++) {
            const parent = state.directoryPromises.get(`${entry.owner}:${parts.slice(0, end).join("/")}`);
            if (parent && !members.has(parent)) dependencies.add(parent);
          }
        }
        // Parents in earlier pages settle first; independent pages overlap.
        // A rejected parent does not revoke unrelated siblings in this page.
        await directoryPreparationWait(Promise.allSettled(dependencies), controller.signal);
        let response;
        for (let failures = 0; ; failures++) {
          if (controller.signal.aborted) throw new DOMException("Folder preparation cancelled", "AbortError");
          if (page.some((entry) => entry.owner !== state.user?.userID)) throw new DOMException("Upload owner changed", "AbortError");
          while (directoryPreparationActive.size >= 4) await directoryPreparationWait(Promise.race(directoryPreparationActive), controller.signal);
          slot = new Promise((resolve) => { release = resolve; });
          directoryPreparationActive.add(slot);
          let retryError;
          try {
            if (page.some((entry) => entry.owner !== state.user?.userID)) throw new DOMException("Upload owner changed", "AbortError");
            response = await api("/api/v1/directories/prepare", { method: "POST", signal: controller.signal, body: { paths: page.map((entry) => entry.path) } });
            break;
          } catch (error) {
            const rollingRoute = error instanceof APIError && error.status === 405;
            if (!navigator.onLine || failures >= transferRetryLimit || !rollingRoute && !transferFailureIsRetryable(error)) throw error;
            retryError = error;
          } finally {
            directoryPreparationActive.delete(slot);
            release();
            slot = null;
          }
          // Ensure revalidates every replay, including a lost success. Backoff
          // holds no HTTP slot, so another owner's pages can make progress.
          await wait(Math.max(retryError.retryAfterMs || 0, transferRetryDelay({ retryCount: failures + 1 })), controller.signal);
        }
        if (page.some((entry) => entry.owner !== state.user?.userID)) throw new DOMException("Upload owner changed", "AbortError");
        const results = response?.directories;
        if (!Array.isArray(results) || results.length !== page.length || results.some((item, index) => item.index !== index || item.path !== page[index].path || typeof item.ready !== "boolean" || item.ready && item.errorCode || !item.ready && !item.errorCode)) throw new TypeError("Folder preparation returned misbound outcomes.");
        for (const [index, entry] of page.entries()) settle(entry, results[index].ready ? null : uploadLifecycleError({ errorKind: results[index].errorCode }));
      } catch (error) { for (const entry of page) settle(entry, error); }
      finally { if (slot) { directoryPreparationActive.delete(slot); release(); } uploadActionControllers.delete(controller); }
    }));
  }

  async function ensureDirectories(baseDirectory, relativeDirectory, preparedDirectories = null, signal = null) {
    const owner = state.user?.userID;
    if (!relativeDirectory) return;
    let current = baseDirectory;
    const pending = [];
    for (const component of relativeDirectory.split("/")) {
      current = joinPath(current, component);
      if (preparedDirectories && preparedDirectories.has(current)) continue;
      const path = current;
      pending.push(ensureDirectory(path, owner, signal).then(() => { if (preparedDirectories && !signal?.aborted) preparedDirectories.add(path); }));
    }
    await Promise.all(pending);
  }

  async function ensureTransferDirectories(transfer, signal = transfer.controller?.signal) {
    const group = transfer.groupID ? state.transferGroups.get(transfer.groupID) : null;
    if (group && !(group.preparedDirectories instanceof Set)) group.preparedDirectories = new Set();
    const controller = new AbortController();
    transfer.directoryPreparationController = controller;
    const abort = () => controller.abort();
    if (signal?.aborted) controller.abort();
    signal?.addEventListener("abort", abort, { once: true });
    try { await ensureDirectories(transfer.baseDirectory, transfer.relativeDirectory, group ? group.preparedDirectories : null, controller.signal); }
    finally {
      signal?.removeEventListener("abort", abort);
      if (transfer.directoryPreparationController === controller) transfer.directoryPreparationController = null;
    }
  }

  function prepareTransferGroup(group) {
    const owner = state.user?.userID;
    group.directoryPreparationController?.abort();
    const controller = new AbortController();
    group.directoryPreparationController = controller;
    uploadActionControllers.add(controller);
    return withUploadNamespaceTurn(() => {
      if (controller.signal.aborted || group.cancelled || owner !== state.user?.userID || state.transferGroups.get(group.id) !== group) return;
      return prepareTransferGroupDirectories(group, controller);
    }).finally(() => {
      uploadActionControllers.delete(controller);
      if (group.directoryPreparationController === controller) group.directoryPreparationController = null;
    });
  }

  async function prepareTransferGroupDirectories(group, controller) {
    group.cancelled = false;
    group.state = "preparing";
    preserveTransferError(group);
    for (const transfer of state.transfers) if (transfer.groupID === group.id && !transfer.preparationReady && !transfer.uploadID && !transfer.pendingCapability && !["complete", "cancelled", "failed"].includes(transfer.state)) setPreparationState(transfer, "directories");
    renderTransfers();
    try {
      const directories = [...group.directories].sort((left, right) => left.split("/").length - right.split("/").length || left.localeCompare(right));
      if (!(group.preparedDirectories instanceof Set)) group.preparedDirectories = new Set();
      const results = await Promise.allSettled(directories.map((relativeDirectory) => ensureDirectories(group.baseDirectory, relativeDirectory, group.preparedDirectories, controller.signal)));
      if (controller.signal.aborted) return;
      for (const [index, result] of results.entries()) if (result.status === "rejected") {
        const failedPath = joinPath(group.baseDirectory, directories[index]);
        for (const transfer of state.transfers) if (transfer.groupID === group.id && !["complete", "cancelled", "failed"].includes(transfer.state)
          && (transfer.directory === failedPath || transfer.directory.startsWith(`${failedPath}/`))) {
          transitionTransfer(transfer, "failed", friendlyError(result.reason, "Folder preparation failed."));
        }
        if (!group.transferIDs.length) throw result.reason;
      }
      if (group.cancelled) return;
      group.state = group.transferIDs.length ? "queued" : "complete";
      if (group.transferIDs.length) updateTransferGroup(group.id, false);
      if (!group.transferIDs.length && state.currentDirectory === group.baseDirectory) loadDirectory(group.baseDirectory);
      pumpTransfers();
    } catch (error) {
      if (controller.signal.aborted) return;
      group.state = "failed";
      group.error = friendlyError(error, "Folder preparation failed.");
      for (const transfer of state.transfers.filter((item) => item.groupID === group.id && !["complete", "cancelled", "failed"].includes(item.state))) transitionTransfer(transfer, "failed", group.error);
      group.failureAnnounced = true;
      announce(`${group.name} failed to upload.`, true);
    }
    renderTransfers();
  }

  function configuredTransferConcurrency(pendingCount) {
    const connection = navigator.connection || navigator.mozConnection || navigator.webkitConnection;
    const pending = Math.max(0, Math.floor(Number(pendingCount) || 0));
    if (pending <= 1 || Boolean(connection && connection.saveData)) return Math.min(1, pending);
    const configuredValue = Number(state.config && state.config.maximumTransferConcurrency);
    const configured = Math.max(1, Number.isFinite(configuredValue) ? Math.floor(configuredValue) : 100);
    return Math.min(configured, pending);
  }

  function automaticTransferConcurrency() {
    const summary = state.transferSummary || aggregateTransferSummary(state.transfers);
    return configuredTransferConcurrency(summary.counts.queued + summary.counts.preparing + summary.counts.uploading);
  }

  async function runTransferControlPool(values, operation) {
    let nextIndex = 0;
    const workers = Array.from({ length: configuredTransferConcurrency(values.length) }, async () => {
      while (nextIndex < values.length) {
        const index = nextIndex;
        nextIndex += 1;
        await operation(values[index]);
      }
    });
    await Promise.all(workers);
  }

  function installUploadWorkerPoolTestFixture() {
    if (!state.config || !state.config.localFixture) return;
    window.__endlessfsUploadSourceTest = {
      beginRateScenario: (size) => {
        if (!Number.isSafeInteger(size) || size <= 0) throw new Error("Invalid rate fixture size");
        const transfer = { id: "rate-fixture", name: "Rate fixture", size, state: "uploading", confirmed: 0, speedBps: 0, lastProgressBytes: 0, lastProgressAt: 0, planPhase: "done", createdAt: Date.now() };
        state.transfers = [transfer];
        state.transferByID = new Map([[transfer.id, transfer]]);
        state.transferGroups.clear();
        uploadRates.clear();
        uploadEstimates.clear();
        beginTransferMeasurement(transfer);
        renderTransfers();
        setTransferSheetOpen(true);
      },
      acknowledgeRateScenario: (bytes) => { recordTransferProgress(state.transferByID.get("rate-fixture"), bytes); refreshUploadRates(); renderTransferProgress(); },
      advanceMeasurement: (milliseconds) => {
        if (!Number.isFinite(milliseconds) || milliseconds < 0) throw new Error("Invalid measurement clock advance");
        uploadMeasurementOffset += milliseconds;
        refreshUploadRates();
        scheduleTransferRender();
        flushUploadCompletions();
      },
      qualify: async (files, url) => {
        const started = performance.now();
        await Promise.all(files.map(async (file, index) => {
          const transfer = { id: `source-qualification-${index}`, file, name: "source qualification", state: "uploading", confirmed: 0, speedBps: 0, lastProgressBytes: 0, lastProgressAt: started, controller: new AbortController() };
          await sendFileData(transfer, { url: `${url}/${index}`, uploadID: transfer.id, method: "PATCH", framing: "offset-header", declaredSize: file.size, chunkRules: { maximumSize: uploadSourceChunkBytes } });
        }));
        return { files: files.length, bytes: files.reduce((sum, file) => sum + file.size, 0), milliseconds: performance.now() - started, peakBytes: uploadSource.peakBytes };
      },
      pauseReads: (paused) => { uploadSource.paused = Boolean(paused); drainUploadSource(); },
      setBufferChunks: (chunks) => {
        if (![1, 2, 4].includes(chunks) || state.activeTransfers || uploadSource.usedBytes || uploadSource.queue.length) throw new Error("Invalid source prototype window");
        uploadSourceBudget = chunks * uploadSourceChunkBytes;
      },
      snapshot: () => ({ usedBytes: uploadSource.usedBytes, peakBytes: uploadSource.peakBytes, readers: uploadSource.readers, peakReaders: uploadSource.peakReaders, waiting: uploadSource.queue.length, finalizing: state.transfers.filter(transfer => !transfer.fixture && transfer.awaitingCompletion).length }),
    };
    window.__endlessfsUploadWorkerPoolTest = {
      seedRecovery: (count, completed = 0) => {
        if (!Number.isSafeInteger(count) || count < 1 || count > 10000 || !Number.isSafeInteger(completed) || completed < 0 || completed > count || state.activeTransfers) throw new Error("Invalid recovery fixture");
        state.transfers = [];
        state.transferByID.clear(); state.transferGroups.clear();
        const group = { id: "recovery-probe", name: "Recovery probe", transferIDs: [], directories: [], baseDirectory: "/", totalSize: count, state: "queued", discoveryDone: true, strategy: "keep-both", createdAt: Date.now() };
        for (let index = 0; index < count; index++) {
          const id = `probe-${index}`, name = `${id}.bin`;
          const transfer = { id, groupID: group.id, name, relativePath: name, directory: "/", baseDirectory: "/", relativeDirectory: "", file: index < completed ? null : new File(["x"], name), size: 1, state: index < completed ? "complete" : "queued", confirmed: index < completed ? 1 : 0, uploadID: index < completed ? "" : `old-${id}`, strategy: "keep-both", planPhase: "done", createdAt: group.createdAt, speedBps: 0, lastProgressAt: 0, lastProgressBytes: 0 };
          state.transfers.push(transfer); state.transferByID.set(id, transfer); group.transferIDs.push(id);
        }
        state.transferGroups.set(group.id, group);
        state.transferQueueCursor = 0;
        renderTransfers(); pumpTransfers();
      },
      concurrency: (pendingCount, saveData) => {
        const pending = Math.max(0, Math.floor(Number(pendingCount) || 0));
        if (pending <= 1 || saveData) return Math.min(1, pending);
        const configuredValue = Number(state.config.maximumTransferConcurrency);
        const configured = Math.max(1, Number.isFinite(configuredValue) ? Math.floor(configuredValue) : 100);
        return Math.min(configured, pending);
      },
      active: () => state.activeTransfers,
      discoveryBatchSize: () => transferDiscoveryBatchSize,
      snapshot: () => ({
        active: state.activeTransfers,
        refreshDirectories: state.uploadRefreshDirectories.size,
        refreshInFlight: state.uploadRefreshInFlight,
        states: state.transfers.reduce((counts, transfer) => {
          counts[transfer.state] = (counts[transfer.state] || 0) + 1;
          return counts;
        }, {}),
        groups: [...state.transferGroups.values()].filter((group) => !group.fixture).map((group) => ({
          name: group.name,
          state: group.state,
          files: group.transferIDs.length,
          complete: group.transferSummary ? group.transferSummary.counts.complete : -1,
        })),
        renderedGroups: Array.from(document.querySelectorAll(".transfer-group-row")).map((row) => row.textContent.trim()),
        renderedFiles: byID("file-rows").textContent.slice(0, 500),
      }),
    };
  }

  function queueUploadDirectoryRefresh(directory) {
    state.uploadRefreshDirectories.add(directory);
    if (state.currentDirectory === "/" || directory.startsWith(`${state.currentDirectory}/`)) state.uploadRefreshDirectories.add(state.currentDirectory);
  }

  async function flushUploadDirectoryRefresh() {
    if (state.uploadRefreshInFlight || state.uploadRefreshDirectories.size === 0) return;
    const directories = new Set(state.uploadRefreshDirectories);
    state.uploadRefreshDirectories.clear();
    if (!directories.has(state.currentDirectory)) return;
    state.uploadRefreshInFlight = true;
    try {
      await loadDirectory(state.currentDirectory);
    } finally {
      state.uploadRefreshInFlight = false;
      if (state.uploadRefreshDirectories.size > 0) flushUploadDirectoryRefresh();
    }
  }

  function beginTransferMeasurement(transfer) {
    transfer.speedBps = 0;
    transfer.lastProgressBytes = Math.min(transfer.confirmed, transferFileSize(transfer));
    transfer.lastProgressAt = uploadMeasurementNow();
    transfer.recoveryFailures = 0;
    if (!transfer.fixture && !uploadRates.has(uploadCohort(transfer))) uploadRates.set(uploadCohort(transfer), { startedAt: uploadMeasurementNow(), samples: [] });
    if (!lastUploadAcknowledgementAt) lastUploadAcknowledgementAt = uploadMeasurementNow();
    if (!uploadProgressTimer) uploadProgressTimer = window.setInterval(() => {
      if (!state.transfers.some((item) => item.state === "uploading" && !item.fixture)) {
        window.clearInterval(uploadProgressTimer);
        uploadProgressTimer = 0;
        uploadRates.clear();
        uploadEstimates.clear();
        lastUploadAcknowledgementAt = 0;
        return;
      }
      refreshUploadRates();
      scheduleTransferRender();
      flushUploadCompletions();
    }, 1000);
  }

  function recordTransferProgress(transfer, confirmed) {
    const priorConfirmed = transfer.confirmed;
    const priorSpeed = transfer.speedBps;
    const next = Math.max(transfer.confirmed, Math.min(confirmed, transferFileSize(transfer)));
    const now = uploadMeasurementNow();
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
    const measure = uploadRates.get(uploadCohort(transfer));
    if (measure && next > priorConfirmed) measure.samples.push({ at: now, bytes: next - priorConfirmed });
    if (!transfer.fixture && next > priorConfirmed) lastUploadAcknowledgementAt = now;
    updateAggregateProgress(state.transferSummary, transfer, priorConfirmed, priorSpeed);
    if (state.transferOverviewIDs.has(transfer.id)) updateAggregateProgress(state.transferOverview, transfer, priorConfirmed, priorSpeed);
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
      if (batch.length > 0) {
        state.uploadBatchActive = true;
        const admissionRun = state.uploadAdmissionGeneration = (state.uploadAdmissionGeneration || 0) + 1;
        admitUploadBatch(batch).finally(() => {
          if (admissionRun !== state.uploadAdmissionGeneration) return;
          state.uploadBatchActive = false;
          scheduleTransferStructureRender();
          pumpTransfers();
        });
      }
    }
	flushUploadCompletions();
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
        flushUploadDirectoryRefresh();
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
      if (transfer.state === "queued" && transfer.file instanceof File && admitted && (!transfer.groupID || state.transferGroups.get(transfer.groupID)?.state !== "preparing")) return transfer;
    }
    return null;
  }

  function nextUploadAdmissionBatch() {
    if (uploadAdmissionRetryTimer || Date.now() < uploadAdmissionRetryAt) return [];
	const replay = state.transfers.find((transfer) => transfer.admissionBatchKey && !transfer.uploadID && !transfer.pendingCapability && transfer.state === "queued" && transfer.file instanceof File);
	if (replay) {
	  return state.transfers
		.filter((transfer) => transfer.admissionBatchKey === replay.admissionBatchKey)
		.sort((left, right) => left.admissionBatchIndex - right.admissionBatchIndex);
	}
    const batch = [];
	let encodedBytes = 64;
    for (const transfer of state.transfers) {
      if (batch.length >= uploadLifecyclePageSize) break;
      if (transfer.state !== "queued" || !(transfer.file instanceof File) || transfer.uploadID || transfer.pendingCapability) continue;
      if (transfer.groupID && state.transferGroups.get(transfer.groupID)?.state === "preparing") continue;
	  if (transfer.admissionBatchKey) continue;
	  const item = transfer.admissionIntent || uploadInitializationRequest(transfer, true);
	  const itemBytes = new TextEncoder().encode(JSON.stringify(item)).byteLength + (batch.length ? 1 : 0);
	  if (encodedBytes + itemBytes > (1 << 20) - 4096) break;
	  encodedBytes += itemBytes;
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
      conflict: transfer.uploadConflict || "rename",
      resumable: true,
    };
    if (request.conflict === "replace" && transfer.targetVersion) request.expectedVersion = transfer.targetVersion;
    if (includeItemKey) request.idempotencyKey = transfer.id;
    return request;
  }

	function compactUploadInitializationRequest(transfer, defaultMediaType, defaultDirectory) {
	  const request = { name: transfer.name, size: transferFileSize(transfer) };
	  if (transfer.directory !== defaultDirectory) request.path = transfer.directory;
	  const mediaType = transferMediaType(transfer);
	  if (mediaType !== defaultMediaType) request.mediaType = mediaType;
	  if ((transfer.uploadConflict || "rename") !== "rename") request.conflict = transfer.uploadConflict;
	  if (request.conflict === "replace" && transfer.targetVersion) request.expectedVersion = transfer.targetVersion;
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
    transitionTransfer(transfer, "failed", friendlyError(error, error.name === "TransferIdentityPersistenceError" ? error.message : "Upload could not be initialized."), error instanceof APIError ? error.code : error.name === "TransferIdentityPersistenceError" ? "local_storage" : error.batchErrorKind || "terminal");
    if (!transfer.groupID) announce(`${transfer.name} failed to upload.`, true);
  }

  function uploadIdentityPersistenceError() {
    const error = new Error("The new upload attempt could not be saved on this device. Free local storage and retry.");
    error.name = "TransferIdentityPersistenceError";
    return error;
  }

  function deferAdmissionRetries(transfers) {
    const pending = transfers.filter((transfer) => !transfer.uploadID && transfer.state === "retry-wait" && !transfer.cancelRequested);
    if (!pending.length) return;
    // Individual retry counters/deadlines remain durable, but the next page
    // waits until the entire retry wave is queued. Timer registration follows
    // all item timers, so one shared wake-up retains page-level batching.
    uploadAdmissionRetryAt = Math.max(uploadAdmissionRetryAt, ...pending.map((transfer) => transfer.nextRetryAt || 0)) + 1;
    window.clearTimeout(uploadAdmissionRetryTimer);
    const wake = () => {
      if (Date.now() < uploadAdmissionRetryAt) { uploadAdmissionRetryTimer = window.setTimeout(wake, uploadAdmissionRetryAt - Date.now()); return; }
      uploadAdmissionRetryAt = 0; uploadAdmissionRetryTimer = 0; pumpTransfers();
    };
    uploadAdmissionRetryTimer = window.setTimeout(wake, Math.max(0, uploadAdmissionRetryAt - Date.now()));
  }

  async function admitUploadBatch(transfers) {
    // Historical envelopes retain their exact ordered membership. New pages
    // bind each item to its own durable ID and may safely retry any subset.
    if (transfers[0]?.admissionBatchKey && transfers[0]?.admissionProtocol !== "independent") return admitLegacyUploadBatch(transfers);
    const owner = state.user?.userID;
    const references = new Map(transfers.map((transfer) => [transfer, { id: transfer.id, generation: transfer.attemptGeneration || 0 }]));
    const current = (transfer) => {
      const reference = references.get(transfer);
      return owner === state.user?.userID && !transfer.cleared && !transfer.suspendedByLogout
        && reference.id === transfer.id && reference.generation === (transfer.attemptGeneration || 0)
        && state.transferByID.get(reference.id) === transfer;
    };
    const controller = new AbortController(); uploadActionControllers.add(controller);
    for (const transfer of transfers) {
      transfer.cancelRequested = false;
      transitionTransfer(transfer, "preparing");
    }
    scheduleTransferStructureRender();
    try {
      await Promise.all(transfers.map(waitForTransferIdentity));
      if (owner !== state.user?.userID || controller.signal.aborted) return;
      await Promise.all(transfers.filter(current).map(async (transfer) => {
        try { await ensureTransferDirectories(transfer, controller.signal); }
        catch (error) { if (current(transfer)) failUploadPreparation(transfer, error); }
      }));
      const intents = new Map();
      for (const transfer of transfers.filter(current)) {
        if (transfer.cancelRequested || transfer.state !== "preparing") continue;
        transfer.admissionProtocol = "independent";
        transfer.durableAdmissionRequired = true;
        transfer.admissionIntent ||= uploadInitializationRequest(transfer, true);
        if (transfer.admissionIntent.idempotencyKey !== transfer.id) throw uploadIdentityPersistenceError();
        intents.set(transfer, { ...transfer.admissionIntent });
        setPreparationState(transfer, "sessions");
      }
      if (!intents.size) return;
      // Intent metadata and its item key must survive a process crash before
      // the POST. Capability persistence may fail later: replay still finds
      // the same server receipt, including already completed siblings.
      if (!await persistTransferItems([...intents.keys()])) throw uploadIdentityPersistenceError();
      let pending = [...intents.keys()].filter((transfer) => current(transfer) && !transfer.cancelRequested);
      if (!pending.length || owner !== state.user?.userID || controller.signal.aborted) return;
      const response = await uploadNamespaceMutation("/api/v1/uploads/batch/admit", {
        method: "POST", signal: controller.signal,
      }, () => {
        // Only independent item receipts permit changing membership. Select
        // live consumers after the namespace wait and bind replies to that
        // exact page; legacy persisted cohorts keep their original envelope.
        pending = pending.filter((transfer) => current(transfer) && !transfer.cancelRequested && transfer.state === "preparing");
        return pending.length ? { uploads: pending.map((transfer) => intents.get(transfer)) } : null;
      });
      if (owner !== state.user?.userID || controller.signal.aborted) return;
      if (!pending.length) return;
      const results = response && Array.isArray(response.uploads) ? response.uploads : [];
      if (results.length !== pending.length || results.some((result, index) => result?.index !== index || result.id !== intents.get(pending[index]).idempotencyKey
        || result.capability && result.capability.uploadID !== result.uploadID
        || result.status && result.status.uploadID !== result.uploadID)) throw new TypeError("Upload admission returned misbound outcomes.");
      const abandoned = [];
      for (const [index, transfer] of pending.entries()) {
        const result = results[index];
        if (!current(transfer) || transfer.cancelRequested || transfer.state === "cancelled") {
          if (["accepted", "already_accepted"].includes(result.outcome) && result.uploadID) abandoned.push(result.uploadID);
          continue;
        }
        if (result.outcome === "completed" && result.status?.state === "completed") {
          transfer.uploadID = result.uploadID; transfer.durableAdmissionRequired = false;
          completeTransferLocally(transfer);
          continue;
        }
        if (["expired", "aborted"].includes(result.outcome)) {
          const error = uploadBatchItemError(result.outcome);
          failUploadPreparation(transfer, error);
          continue;
        }
        const accepted = ["accepted", "already_accepted"].includes(result.outcome) && result.uploadID;
        const capacityDeferred = result.capacityDeferred === true && result.nextAction === "resume" && !result.capability && result.status;
        if (accepted && (capacityDeferred || result.detailsUnavailable === true)) {
          // Admission already succeeded. Persist its identity even if its
          // opaque capability needs a smaller resume response or attention.
          transfer.uploadID = result.uploadID; transfer.durableAdmissionRequired = false;
        }
        if (result.errorKind || !accepted || !result.capability && !result.status?.dataComplete && !capacityDeferred) {
          const error = result.errorKind ? uploadLifecycleError(result) : uploadBatchItemError("internal");
          failUploadPreparation(transfer, error);
          continue;
        }
        transfer.uploadID = result.uploadID; transfer.durableAdmissionRequired = false;
        transfer.batchID = result.capability?.batchID || "";
        transfer.batchIndex = result.capability?.batchIndex || 0; transfer.batchCount = result.capability?.batchCount || 0;
        transfer.pendingCapability = result.capability || null;
        transfer.pendingAdmissionStatus = result.status || null;
        restoreTransferProgress(transfer, result.status?.dataComplete ? transferFileSize(transfer) : result.status?.confirmedOffset || 0);
        transitionTransfer(transfer, "queued");
      }
      deferAdmissionRetries(pending.filter(current));
      if (abandoned.length && owner === state.user?.userID && !controller.signal.aborted) {
        await uploadNamespaceMutation("/api/v1/uploads/batch", { method: "DELETE", signal: controller.signal,
          headers: { "Idempotency-Key": idempotencyKey() }, body: { uploadIDs: abandoned } }).catch(() => {});
      }
      if (owner === state.user?.userID) await persistTransferItems(pending.filter(current));
    } catch (error) {
      if (owner !== state.user?.userID || error.name === "AbortError") return;
      const failure = error instanceof APIError && [404, 405].includes(error.status) ? uploadBatchItemError("unavailable") : error;
      for (const transfer of transfers.filter(current)) if (!transfer.uploadID) failUploadPreparation(transfer, failure);
      deferAdmissionRetries(transfers.filter(current));
    } finally { uploadActionControllers.delete(controller); }
  }

  async function admitLegacyUploadBatch(transfers) {
    const owner = state.user?.userID;
    const references = new Map(transfers.map((transfer) => [transfer, { id: transfer.id, generation: transfer.attemptGeneration || 0 }]));
    const current = (transfer) => {
      const reference = references.get(transfer);
      return owner === state.user?.userID && !transfer.cleared && !transfer.suspendedByLogout
        && reference.id === transfer.id && reference.generation === (transfer.attemptGeneration || 0)
        && state.transferByID.get(reference.id) === transfer;
    };
    const controller = new AbortController();
    uploadActionControllers.add(controller);
    const existingKey = transfers[0]?.admissionBatchKey || "";
    for (const transfer of transfers) {
      if (!existingKey) transfer.cancelRequested = false;
      if (!["complete", "cancelled", "failed"].includes(transfer.state)) transitionTransfer(transfer, "preparing");
    }
    scheduleTransferStructureRender();
    try {
      await Promise.all(transfers.map(waitForTransferIdentity));
      if (owner !== state.user?.userID || controller.signal.aborted) return;
      if (!existingKey) await Promise.all(transfers.filter(current).map((transfer) => ensureTransferDirectories(transfer, controller.signal)));
      if (owner !== state.user?.userID || controller.signal.aborted) return;
      const pending = existingKey ? transfers : transfers.filter((transfer) => current(transfer) && transfer.state === "preparing" && !transfer.cancelRequested);
      if (!pending.length) return;
      for (const transfer of pending) if (current(transfer)) setPreparationState(transfer, "sessions");
      const batchKey = existingKey || idempotencyKey();
      if (!existingKey) {
        for (const [index, transfer] of pending.entries()) {
          transfer.admissionBatchKey = batchKey;
          transfer.admissionBatchIndex = index;
          transfer.admissionBatchCount = pending.length;
        }
      }
      // Capture the complete original intent before any asynchronous ledger
      // write. Replays retain ordered membership and header-derived item keys.
      const body = {
        defaults: { path: pending[0].directory, mediaType: transferMediaType(pending[0]), conflict: "rename", resumable: true },
        uploads: pending.map((transfer) => compactUploadInitializationRequest(transfer, transferMediaType(pending[0]), pending[0].directory)),
      };
      const durableAdmissionRequired = pending.some((transfer) => transfer.durableAdmissionRequired);
      if (!existingKey || durableAdmissionRequired) {
        const saved = await persistTransferItems(pending);
        if (durableAdmissionRequired && !saved) throw uploadIdentityPersistenceError();
      }
      if (owner !== state.user?.userID || controller.signal.aborted) return;
      const response = await uploadNamespaceMutation("/api/v1/uploads/batch", {
        method: "POST", signal: controller.signal,
        headers: { "Idempotency-Key": batchKey }, body,
      });
      if (owner !== state.user?.userID || controller.signal.aborted) return;
      const results = response && Array.isArray(response.uploads) ? response.uploads : [];
      if (results.length !== pending.length) throw uploadBatchItemError("internal");
      const abandoned = [];
      for (const [offset, transfer] of pending.entries()) {
        const result = results[offset];
        if (!current(transfer) || transfer.cancelRequested || transfer.state === "cancelled") {
          if (result?.index === offset && result.capability?.uploadID) abandoned.push(result.capability.uploadID);
          if (current(transfer)) {
            transfer.admissionBatchKey = ""; transfer.admissionBatchIndex = 0; transfer.admissionBatchCount = 0;
          }
          continue;
        }
        if (!result || result.index !== offset || !result.capability) {
          failUploadPreparation(transfer, uploadBatchItemError(result && result.errorKind));
          continue;
        }
        if (transfer.uploadID && transfer.uploadID !== result.capability.uploadID) throw uploadBatchItemError("conflict");
        transfer.uploadID = result.capability.uploadID;
        transfer.durableAdmissionRequired = false;
        transfer.admissionBatchKey = "";
        transfer.admissionBatchIndex = 0;
        transfer.admissionBatchCount = 0;
        transfer.batchID = result.capability.batchID || "";
        transfer.batchIndex = Number.isSafeInteger(result.capability.batchIndex) ? result.capability.batchIndex : 0;
        transfer.batchCount = Number.isSafeInteger(result.capability.batchCount) ? result.capability.batchCount : 0;
        transfer.pendingCapability = result.capability;
        transitionTransfer(transfer, "queued");
      }
      // A cancelled/replaced attempt may have been admitted before its reply
      // arrived. Dispose only those old sessions under the original owner.
      deferAdmissionRetries(pending.filter(current));
      if (abandoned.length && owner === state.user?.userID && !controller.signal.aborted) {
        await uploadNamespaceMutation("/api/v1/uploads/batch", { method: "DELETE", signal: controller.signal,
          headers: { "Idempotency-Key": `${batchKey}-stale-cancel` }, body: { uploadIDs: abandoned } }).catch(() => {});
      }
      if (owner === state.user?.userID) await persistTransferItems(pending.filter(current));
    } catch (error) {
      if (owner !== state.user?.userID || error.name === "AbortError") return;
      for (const transfer of transfers.filter(current)) failUploadPreparation(transfer, error);
      deferAdmissionRetries(transfers.filter(current));
    } finally {
      uploadActionControllers.delete(controller);
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
    if (state.transferOverviewIDs.has(transfer.id)) updateAggregateState(state.transferOverview, transfer, priorState, nextState, priorSpeed);
    updateAggregateState(state.transferGroups.get(transfer.groupID)?.transferSummary, transfer, priorState, nextState, priorSpeed);
    queueTransferPersistence(transfer);
    updateTransferGroup(transfer.groupID, false);
  }

  function transferFailureIsRetryable(error) {
    if (error?.batchRetryable === false) return false;
    if (!navigator.onLine) return true;
    if (error instanceof APIError) return error.status === 408 || error.status === 425 || error.status === 429 || error.status >= 500;
    if (error && ["rate_limited", "unavailable", "internal"].includes(error.batchErrorKind)) return true;
    return error instanceof TypeError || error instanceof SyntaxError || (error && error.name === "NetworkError");
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
    const delay = Math.max(transferRetryDelay(transfer), error.retryAfterMs || 0, state.transferCircuitOpenUntil - Date.now());
    transfer.nextRetryAt = Date.now() + delay;
    transitionTransfer(transfer, "retry-wait", "Retrying automatically.", "transient");
    window.clearTimeout(state.transferRetryTimers.get(transfer.id));
    const generation = transfer.attemptGeneration || 0;
    state.transferRetryTimers.set(transfer.id, window.setTimeout(() => {
      state.transferRetryTimers.delete(transfer.id);
      if (transfer.state !== "retry-wait" || generation !== (transfer.attemptGeneration || 0)) return;
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
      if (transfer.planPhase && transfer.planPhase !== "done") continue;
      transitionTransfer(transfer, "queued");
      resumed += 1;
    }
    resumeUploadPlans();
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
    const generation = transfer.attemptGeneration || 0;
    const owner = state.user?.userID;
    const localID = transfer.id;
    updateTransferGroup(transfer.groupID, false);
    scheduleTransferStructureRender();
    try {
      let capability = transfer.pendingCapability || null;
      const admissionStatus = transfer.pendingAdmissionStatus || null;
      transfer.pendingCapability = null; transfer.pendingAdmissionStatus = null;
      const mediaType = transferMediaType(transfer);
      const size = transferFileSize(transfer);
      if (!capability && !admissionStatus?.dataComplete) {
		if (transfer.uploadID) {
          setPreparationState(transfer, "recovery");
          const item = await uploadLifecycle(transfer, true);
          if (item.errorKind) throw uploadLifecycleError(item);
          const status = item.status;
          if (!status) throw new TypeError("Upload recovery omitted status.");
          if (status.state === "completed") {
            restoreTransferProgress(transfer, size);
            transitionTransfer(transfer, "complete");
            queueUploadDirectoryRefresh(transfer.directory);
            return;
          }
          if (status.state === "expired" || status.state === "aborted") {
            if (transfer.allowExpiredRestart) {
              restartExpiredTransfer(transfer);
              transitionTransfer(transfer, transfer.planPhase && transfer.planPhase !== "done" ? "preparing" : "queued");
              resumeUploadPlans();
            } else transitionTransfer(transfer, "failed", "Upload session expired. Retry to start a new attempt.", status.state);
            return;
          }
          restoreTransferProgress(transfer, status.dataComplete ? size : status.confirmedOffset);
          capability = item.capability || null;
          if (!capability && !status.dataComplete) throw new TypeError("Upload recovery omitted its capability.");
		} else {
          await waitForTransferIdentity(transfer);
          if (transfer.controller.signal.aborted) throw new DOMException("Upload preparation cancelled", "AbortError");
          setPreparationState(transfer, "directories");
		  await ensureTransferDirectories(transfer);
          setPreparationState(transfer, "sessions");
		  capability = await uploadNamespaceMutation("/api/v1/uploads", { method: "POST", headers: { "Idempotency-Key": transfer.id }, body: uploadInitializationRequest(transfer), signal: transfer.controller.signal });
		}
      }
      if (owner !== state.user?.userID || localID !== transfer.id || generation !== (transfer.attemptGeneration || 0) || transfer.controller?.signal.aborted) throw new DOMException("Upload attempt replaced", "AbortError");
	  if (capability) {
        transfer.durableAdmissionRequired = false;
		transfer.uploadID = capability.uploadID;
		transfer.batchID = capability.batchID || transfer.batchID || "";
		transfer.batchIndex = Number.isSafeInteger(capability.batchIndex) ? capability.batchIndex : (transfer.batchIndex || 0);
		transfer.batchCount = Number.isSafeInteger(capability.batchCount) ? capability.batchCount : (transfer.batchCount || 0);
	  }
      transitionTransfer(transfer, "uploading");
      beginTransferMeasurement(transfer);
      updateTransferGroup(transfer.groupID, false);
      scheduleTransferStructureRender();
	  const fingerprintPromise = (transfer.md5 && transfer.crc32c
		? Promise.resolve({ md5: transfer.md5, crc32c: transfer.crc32c })
		: fingerprintUploadFile(transfer)).then((fingerprint) => {
        if (generation !== (transfer.attemptGeneration || 0) || transfer.controller?.signal.aborted) throw new DOMException("Upload preparation replaced", "AbortError");
        if (transfer.sourceExpectedMD5 && transfer.sourceExpectedMD5 !== fingerprint.md5 || transfer.sourceExpectedCRC32C && transfer.sourceExpectedCRC32C !== fingerprint.crc32c) {
          const mismatch = new Error("The selected source differs from the original file. Reconnect the original source.");
          mismatch.name = "SourceMismatchError";
          throw mismatch;
        }
        transfer.md5 = fingerprint.md5; transfer.crc32c = fingerprint.crc32c;
        setPreparationState(transfer, "ready", true);
        return fingerprint;
      });
      setPreparationState(transfer, "fingerprints");
	  // Consume worker failure immediately, even while a slow source read is
	  // pending. The original promise is still awaited for integrity below.
	  fingerprintPromise.catch(() => {});
      if (transfer.sourceExpectedMD5 || transfer.sourceExpectedCRC32C) await fingerprintPromise;
	  if (capability) await sendFileData(transfer, capability);
	  const fingerprint = await fingerprintPromise;
	  transfer.md5 = fingerprint.md5;
	  transfer.crc32c = fingerprint.crc32c;
      recordTransferProgress(transfer, size);
      transfer.retryCount = 0;
      transfer.nextRetryAt = 0;
	  transfer.awaitingCompletion = true;
	  // Fresh independent attempts retain per-upload terminal receipts and
	  // finalize in the same bounded pages as admission/recovery. Previously
	  // persisted atomic cohorts retain their exact key and membership.
	  if (transfer.admissionProtocol === "independent" && !transfer.completionBatchKey) transfer.completionProtocol = "actions";
	  transfer.completionReadyAt = uploadMeasurementNow();
	  transfer.error = "";
	  transfer.speedBps = 0;
	  queueTransferPersistence(transfer);
    } catch (error) {
      if (generation !== (transfer.attemptGeneration || 0)) return;
      cancelUploadFingerprint(transfer);
      if (error.name === "AbortError" && transfer.suspendedByLogout) {
        return;
      } else if (error.name === "AbortError" && transfer.cancelRequested) {
        transitionTransfer(transfer, "cancelled", "Cancelled", "cancelled");
      } else if (error.name === "SourceReadError" || error.name === "SourceMismatchError") {
        setPreparationState(transfer, "source");
        if (error.name === "SourceMismatchError") transfer.file = null;
        transitionTransfer(transfer, "needs-source", error.message, error.name === "SourceMismatchError" ? "source_mismatch" : "source_read");
      } else if (await reconcileUploadAdmissionConflict(transfer, error)) {
        return;
      } else if (transferFailureIsRetryable(error)) {
        scheduleTransferRetry(transfer, error);
      } else {
        transitionTransfer(transfer, "failed", friendlyError(error, error.name === "TransferIdentityPersistenceError" ? error.message : "Upload could not continue."), error instanceof APIError ? error.code : error.name === "TransferIdentityPersistenceError" ? "local_storage" : error.batchErrorKind || "terminal");
        if (!transfer.groupID) announce(`${transfer.name} failed to upload.`, true);
      }
    } finally {
      if (generation === (transfer.attemptGeneration || 0)) transfer.controller = null;
      scheduleTransferStructureRender();
    }
  }

	function completionCandidates() {
	  const ready = state.transfers.filter((transfer) => transfer.awaitingCompletion && transfer.uploadID && transfer.crc32c && !transfer.cancelRequested && transfer.state === "uploading");
      const legacy = ready.filter((transfer) => transfer.completionProtocol !== "actions");
      if (!legacy.length) return ready.slice(0, uploadLifecyclePageSize);
	  const persistedKey = legacy.find((transfer) => transfer.completionBatchKey)?.completionBatchKey || "";
	  const candidates = legacy.filter((transfer) => !persistedKey || transfer.completionBatchKey === persistedKey).slice(0, 10000);
	  if (persistedKey) {
		if (candidates.every((transfer) => Number.isSafeInteger(transfer.completionBatchIndex) && transfer.completionBatchIndex >= 0)) {
		  candidates.sort((left, right) => left.completionBatchIndex - right.completionBatchIndex);
		} else {
		  // Existing ledgers already retain folder discovery order. Recover
		  // that order for pending single-folder batches written before the
		  // explicit completion ordinal was recorded.
		  const group = state.transferGroups.get(candidates[0]?.groupID);
		  if (group && candidates.every((transfer) => transfer.groupID === group.id)) {
			const order = new Map(group.transferIDs.map((id, index) => [id, index]));
			candidates.sort((left, right) => order.get(left.id) - order.get(right.id));
		  }
		}
	  }
	  return candidates;
	}

  const uploadActionControllers = new Set();
  function suspendUploadControl() {
    for (const group of state.transferGroups.values()) group.directoryPreparationController?.abort();
    for (const job of uploadLifecycleQueue) job.abort();
    uploadLifecycleQueue = [];
    if (activeUploadLifecyclePage) {
      for (const job of activeUploadLifecyclePage.jobs) job.abort();
      activeUploadLifecyclePage.controller.abort();
    }
    window.clearTimeout(uploadLifecycleRetryTimer);
    uploadLifecycleRetryTimer = 0;
    uploadLifecycleRetryAt = 0;
    for (const controller of uploadActionControllers) controller.abort();
    window.clearTimeout(uploadAdmissionRetryTimer);
    uploadAdmissionRetryTimer = 0; uploadAdmissionRetryAt = 0;
    state.uploadAdmissionGeneration = (state.uploadAdmissionGeneration || 0) + 1;
    state.uploadBatchActive = false;
    state.uploadCompletionGeneration = (state.uploadCompletionGeneration || 0) + 1;
    state.uploadCompletionActive = false;
    window.clearTimeout(state.uploadCompletionTimer);
    state.uploadCompletionTimer = 0;
  }

  function completeTransferLocally(transfer) {
    transfer.awaitingCompletion = false;
    transfer.pendingAction = "";
    transfer.completionProtocol = "";
    transfer.completionBatchKey = "";
    transfer.completionBatchIndex = -1;
    transfer.retryCount = 0;
    transfer.nextRetryAt = 0;
    restoreTransferProgress(transfer, transferFileSize(transfer));
    transitionTransfer(transfer, "complete");
    queueUploadDirectoryRefresh(transfer.directory);
  }

  async function applyUploadActions(transfers, action) {
    const owner = state.user?.userID;
    let retryDelay = 0;
    for (let first = 0; first < transfers.length; first += uploadLifecyclePageSize) {
      const batch = transfers.slice(first, first + uploadLifecyclePageSize);
      const generations = batch.map((transfer) => transfer.attemptGeneration || 0);
      const ids = batch.map((transfer) => transfer.uploadID);
      const items = batch.map((transfer,index) => ({ uploadID: ids[index], action, ...(action === "complete" ? { crc32c: transfer.crc32c } : {}) }));
      const controller = new AbortController();
      uploadActionControllers.add(controller);
      try {
        const response = await withUploadNamespaceTurn(() => api("/api/v1/uploads/batch/actions", { method: "POST", signal: controller.signal, body: { items } }));
        if (!Array.isArray(response?.uploads) || response.uploads.length !== batch.length || response.uploads.some((item, index) => item.index !== index || item.uploadID !== ids[index] || item.action !== action || !["applied", "already_applied", "already_completed", "rejected", "retryable", "unknown"].includes(item.outcome))) throw new TypeError("Upload action returned an incomplete or misbound page.");
        for (const [index, transfer] of batch.entries()) {
          if (owner !== state.user?.userID || transfer.cleared || generations[index] !== (transfer.attemptGeneration || 0) || ids[index] !== transfer.uploadID) continue;
          const item = response.uploads[index];
          if (item.outcome === "already_completed" || action === "complete" && ["applied", "already_applied"].includes(item.outcome)) {
            completeTransferLocally(transfer);
          } else if (action === "cancel" && ["applied", "already_applied"].includes(item.outcome)) {
            transfer.pendingAction = "";
            transfer.awaitingCompletion = false;
            transitionTransfer(transfer, "cancelled", "Cancelled", "cancelled");
          } else if (action === "complete" && item.retryable) {
            transfer.completionProtocol = "actions";
            transfer.completionBatchKey = "";
            transfer.completionBatchIndex = -1;
            transfer.retryCount = (transfer.retryCount || 0) + 1;
            if (transfer.retryCount > transferRetryLimit) transitionTransfer(transfer, "failed", "Finalization could not be confirmed. Retry the affected item.", "retry_exhausted");
            else retryDelay = Math.max(retryDelay, transferRetryDelay(transfer));
            queueTransferPersistence(transfer);
          } else {
            if (action === "cancel") transfer.pendingAction = "cancel";
            if (action === "complete" && item.nextAction === "restart") transfer.awaitingCompletion = false;
            const code = item.status?.state === "expired" ? "expired" : item.status?.state === "aborted" ? "aborted" : item.errorKind || "terminal";
            transitionTransfer(transfer, "failed", code === "expired" ? "Upload session expired. Retry to start a new attempt." : action === "cancel" ? "Cancellation could not be confirmed. Retry cancellation." : "Uploaded data could not be finalized. Retry the affected item.", code);
          }
        }
        await persistTransferItems(batch);
      } finally { uploadActionControllers.delete(controller); }
    }
    if (retryDelay) scheduleUploadCompletionRetry(retryDelay);
  }

	function scheduleUploadCompletionRetry(delay = 1000) {
	  if (state.uploadCompletionTimer) return;
	  state.uploadCompletionTimer = window.setTimeout(() => {
		state.uploadCompletionTimer = 0;
		flushUploadCompletions();
	  }, delay);
	}

	async function flushUploadCompletions() {
	  if (!navigator.onLine || state.uploadCompletionActive || state.uploadCompletionTimer) return;
	  const readyCount = state.transfers.reduce((count, transfer) => count + (transfer.awaitingCompletion && transfer.uploadID && transfer.crc32c && !transfer.cancelRequested ? 1 : 0), 0);
	  const readyAt = state.transfers.filter((transfer) => transfer.awaitingCompletion && !transfer.cancelRequested).reduce((oldest, transfer) => Math.min(oldest, transfer.completionReadyAt ?? uploadMeasurementNow()), Infinity);
	  // Age verified work independently of other files' acknowledgements, and
	  // flush at one existing durable progress segment. Short bursts coalesce.
	  const readyPageSize = state.transfers.some((transfer) => transfer.awaitingCompletion && transfer.completionProtocol === "actions") ? uploadLifecyclePageSize : 1000;
	  if (state.activeTransfers !== 0 && readyCount < readyPageSize && uploadMeasurementNow() - readyAt < 15000 && uploadMeasurementNow() - lastUploadAcknowledgementAt < 15000) return;
	  const transfers = completionCandidates();
	  if (!transfers.length) return;
	  state.uploadCompletionActive = true;
	  const owner = state.user?.userID;
      const generations = transfers.map((transfer) => transfer.attemptGeneration || 0);
      const completionRun = state.uploadCompletionGeneration = (state.uploadCompletionGeneration || 0) + 1;
      const controller = new AbortController();
      uploadActionControllers.add(controller);
	  const independent = transfers[0].completionProtocol === "actions";
	  const existingKey = transfers.find((transfer) => transfer.completionBatchKey)?.completionBatchKey || "";
	  const batchKey = existingKey || idempotencyKey();
	  const completionItems = transfers.map((transfer) => ({ uploadID: transfer.uploadID, crc32c: transfer.crc32c }));
	  for (const [index, transfer] of transfers.entries()) if (!independent) {
		transfer.completionBatchKey = batchKey;
		transfer.completionBatchIndex = index;
	  }
	  await persistTransferItems(transfers);
	  try {
        if (independent) { await applyUploadActions(transfers, "complete"); return; }
		const result = await withUploadNamespaceTurn(() => api("/api/v1/uploads/batch/complete", {
		  method: "POST", signal: controller.signal, headers: { "Idempotency-Key": batchKey },
		  body: { uploads: completionItems },
		}));
		if (!result || !Array.isArray(result.entries) || result.entries.length !== transfers.length) throw new TypeError("Upload completion returned an invalid response.");
		for (const [index, transfer] of transfers.entries()) {
          if (owner !== state.user?.userID || generations[index] !== (transfer.attemptGeneration || 0) || transfer.cancelRequested) continue;
		  transfer.awaitingCompletion = false;
		  transfer.completionBatchKey = "";
		  transfer.completionBatchIndex = -1;
		  transfer.retryCount = 0;
		  transfer.nextRetryAt = 0;
		  transitionTransfer(transfer, "complete");
		  queueUploadDirectoryRefresh(transfer.directory);
		  if (!transfer.groupID) announce(`${transfer.name} uploaded.`);
		}
	  } catch (error) {
        if (owner !== state.user?.userID || error.name === "AbortError") return;
        const current = transfers.filter((transfer, index) => !transfer.cleared && !transfer.cancelRequested && generations[index] === (transfer.attemptGeneration || 0));
        if (!current.length) return;
        if (error instanceof APIError && [404, 409, 412].includes(error.status)) {
          // The legacy cohort was rejected. Independent UploadID receipts can
          // now classify/apply each remaining item without splitting an
          // ambiguous legacy request or republishing successful siblings.
          for (const transfer of current) { transfer.completionProtocol = "actions"; transfer.completionBatchKey = ""; transfer.completionBatchIndex = -1; }
          await persistTransferItems(current);
          try { await applyUploadActions(current, "complete"); }
          catch (recoveryError) {
            if (recoveryError.name !== "AbortError" && owner === state.user?.userID) {
              for (const transfer of current) {
                transfer.retryCount = (transfer.retryCount || 0) + 1;
                if (transfer.retryCount > transferRetryLimit || !transferFailureIsRetryable(recoveryError)) transitionTransfer(transfer, "failed", "Finalization could not be confirmed. Retry the affected item.", "retry_exhausted");
                queueTransferPersistence(transfer);
              }
              if (current.some((transfer) => transfer.state === "uploading")) scheduleUploadCompletionRetry(Math.max(recoveryError.retryAfterMs || 0, 1000));
            }
          }
          return;
        }
		if (transferFailureIsRetryable(error)) {
		  for (const transfer of current) {
			transfer.retryCount = (transfer.retryCount || 0) + 1;
			transfer.error = "";
			if (transfer.retryCount > transferRetryLimit) transitionTransfer(transfer, "failed", "Finalization could not be confirmed. Retry the affected item.", "retry_exhausted");
			queueTransferPersistence(transfer);
		  }
		  if (current.some((transfer) => transfer.state === "uploading")) scheduleUploadCompletionRetry(Math.max(error.retryAfterMs || 0, Math.min(60000, 1000 * (2 ** Math.min(6, Math.max(...current.map((transfer) => transfer.retryCount)))))));
		} else {
		  for (const transfer of current) transitionTransfer(transfer, "failed", friendlyError(error, "Uploaded data could not be finalized."), error instanceof APIError ? error.code : "terminal");
		  announce(`${transfers.length} uploaded files could not be finalized.`, true);
		}
	  } finally {
        uploadActionControllers.delete(controller);
        if (completionRun === state.uploadCompletionGeneration) state.uploadCompletionActive = false;
		if (owner === state.user?.userID) {
		for (const transfer of transfers) updateTransferGroup(transfer.groupID, false);
		rebuildTransferProjection();
		renderTransfers();
		flushUploadDirectoryRefresh();
		if (!state.uploadCompletionTimer) flushUploadCompletions();
		}
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
      const source = await readUploadSource(transfer, offset, end);
      try {
        response = await fetch(capability.url, { method: capability.method, headers, body: source.body, signal: transfer.controller.signal });
      } catch (error) {
        if (error.name === "AbortError") throw error;
        response = null;
      } finally {
        source.release();
      }
      if (response && (response.ok || (framing === "content-range" && response.status === 308))) {
        const range = response.headers.get("Range") || "";
        const rangeMatch = /^bytes=0-([0-9]+)$/.exec(range);
        let confirmed = Number.parseInt(response.headers.get("Upload-Offset") || "", 10);
        if (rangeMatch) confirmed = Number.parseInt(rangeMatch[1], 10) + 1;
        if (!Number.isSafeInteger(confirmed) && response.ok) confirmed = end;
        if (!Number.isSafeInteger(confirmed)) {
          const status = await readUploadStatus(transfer);
          confirmed = status.confirmedOffset;
        }
        if (!Number.isSafeInteger(confirmed) || confirmed < offset || confirmed > end) throw new Error("Upload returned an invalid confirmed offset.");
        failures = confirmed > offset || size === 0 ? 0 : failures + 1;
        offset = confirmed;
        recordTransferProgress(transfer, offset);
        transfer.recoveryFailures = failures;
        if (failures > 3) throw new TypeError("Upload made no progress after three recovery attempts.");
        scheduleTransferRender();
        if (size === 0) break;
        if (failures) await new Promise((resolve) => window.setTimeout(resolve, 250 * (2 ** (failures - 1))));
        continue;
      }
      const status = await readUploadStatus(transfer);
      if (status.state === "completed" || status.dataComplete) {
        recordTransferProgress(transfer, size);
        return;
      }
      if (status.state === "aborted" || status.state === "expired") {
        const terminal = new Error(status.state === "expired" ? "Upload expired." : "Upload was cancelled.");
        terminal.name = "TerminalUploadError";
        terminal.batchErrorKind = status.state;
        throw terminal;
      }
      const priorOffset = offset;
      offset = status.confirmedOffset;
      recordTransferProgress(transfer, offset);
      failures = offset > priorOffset ? 0 : failures + 1;
      transfer.recoveryFailures = failures;
      scheduleTransferRender();
      if (failures > 3) throw new TypeError("Upload interrupted after three recovery attempts.");
      if (failures) await new Promise((resolve) => window.setTimeout(resolve, 250 * (2 ** (failures - 1)) + Math.floor(Math.random() * 100)));
    }
  }

  async function cancelTransfer(transfer) {
    transfer.cancelRequested = true;
    transfer.directoryPreparationController?.abort();
    cancelUploadFingerprint(transfer);
    if (!transfer.groupID) state.transferPlanControllers.get(transfer.id)?.abort();
    if (!transfer.groupID) window.clearTimeout(state.uploadPlanRetryTimers.get(transfer.id));
    if (!transfer.groupID) state.uploadPlanRetryTimers.delete(transfer.id);
    if (transfer.controller) transfer.controller.abort();
    transfer.attemptGeneration = (transfer.attemptGeneration || 0) + 1;
    transfer.pendingAction = transfer.uploadID ? "cancel" : "";
    transitionTransfer(transfer, "cancelled", "Cancelled", "cancelled");
    if (transfer.uploadID) {
	  const owner = state.user?.userID, generation = transfer.attemptGeneration, uploadID = transfer.uploadID;
	  const current = () => owner === state.user?.userID && generation === transfer.attemptGeneration && uploadID === transfer.uploadID && !transfer.cleared;
	  const body = { uploadIDs: [transfer.uploadID] };
	  if (transfer.batchID && transfer.batchCount === 1) body.batchID = transfer.batchID;
	  try { await uploadNamespaceMutation("/api/v1/uploads/batch", { method: "DELETE", headers: { "Idempotency-Key": `${transfer.id}-cancel` }, body }); if (current()) { transfer.pendingAction = ""; queueTransferPersistence(transfer); } }
      catch { if (current()) await retryPendingCancellation([transfer]); }
    }
    rebuildTransferProjection();
    renderTransfers();
    announce(`${transfer.name} cancelled.`);
  }

  async function cleanFailedUploadSessions(transfers) {
    const allocated = transfers.filter((transfer) => !transfer.fixture && transfer.uploadID);
    const groups = new Map();
    for (const transfer of allocated) {
      const key = transfer.batchID || "";
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(transfer);
    }
    for (const [batchID, members] of groups) {
      const count = members[0].batchCount;
      const indices = new Set(members.map((transfer) => transfer.batchIndex));
      const wholeBatch = batchID && count === members.length && indices.size === count
        && members.every((transfer) => transfer.batchCount === count && transfer.batchIndex >= 0 && transfer.batchIndex < count);
      const envelope = wholeBatch ? { batchID } : {};
      for (const batch of uploadControlBatches(members, (transfer) => transfer.uploadID, "uploadIDs", envelope)) {
        const body = { uploadIDs: batch.map((transfer) => transfer.uploadID) };
        if (wholeBatch && batch.length === members.length) body.batchID = batchID;
        try {
          await uploadNamespaceMutation("/api/v1/uploads/batch", {
            method: "DELETE", headers: { "Idempotency-Key": `${batch[0].id}-clear-failed` },
            body,
          });
        } catch (error) {
          // A lost completion/abort response can leave a terminal upload in
          // failed local history. Verify it; never mistake a permission or
          // provider failure for successful cleanup.
          if (!(error instanceof APIError) || ![404, 409].includes(error.status)) throw error;
          const statuses = await Promise.all(batch.map(async (transfer) => {
            try { return await readUploadStatus(transfer); }
            catch (lookupError) {
              if (lookupError.batchErrorKind === "not_found" || lookupError instanceof APIError && lookupError.status === 404) return null;
              throw lookupError;
            }
          }));
          const unfinished = batch.filter((transfer, index) => statuses[index] && !["completed", "aborted"].includes(statuses[index].state));
          if (unfinished.length) {
            await uploadNamespaceMutation("/api/v1/uploads/batch", {
              method: "DELETE", headers: { "Idempotency-Key": `${batch[0].id}-clear-failed-remainder` },
              body: { uploadIDs: unfinished.map((transfer) => transfer.uploadID) },
            });
          }
        }
      }
    }
  }

  async function clearFailedTransfers(candidates) {
    const ownerID = state.transferLedgerOwner;
    const failed = candidates.filter((transfer) => transfer.state === "failed" && !transfer.clearing && !transfer.cleared);
    if (!failed.length) return;
    for (const transfer of failed) transfer.clearing = true;
    renderTransfers();
    const removedGroups = [];
    try {
      await cleanFailedUploadSessions(failed);
      if (state.transferLedgerOwner !== ownerID) return;
      // Tombstone live references before deletion, including delayed ledger
      // writers. IndexedDB orders prior transactions before this deletion.
      for (const transfer of failed) {
        transfer.cleared = true;
        state.transferPersistQueue.delete(transfer.id);
        window.clearTimeout(state.transferRetryTimers.get(transfer.id));
        state.transferRetryTimers.delete(transfer.id);
        cancelUploadFingerprint(transfer);
      }
      const removedIDs = new Set(failed.map((transfer) => transfer.id));
      for (const groupID of new Set(failed.map((transfer) => transfer.groupID).filter(Boolean))) {
        const group = state.transferGroups.get(groupID);
        if (group && group.discoveryDone !== false && group.transferIDs.every((id) => removedIDs.has(id))) {
          group.cleared = true;
          removedGroups.push(groupID);
        }
      }
      await deleteTransferLedgerEntries([...removedIDs], removedGroups, ownerID);
      if (state.transferLedgerOwner !== ownerID) return;
      state.transfers = state.transfers.filter((transfer) => !removedIDs.has(transfer.id));
      for (const id of removedIDs) {
        state.transferByID.delete(id);
        state.expandedTransferFailures.delete(id);
      }
      for (const groupID of removedGroups) {
        state.transferGroups.delete(groupID);
        state.expandedTransferGroups.delete(groupID);
        window.clearTimeout(state.uploadPlanRetryTimers.get(groupID));
        state.uploadPlanRetryTimers.delete(groupID);
      }
      for (const group of state.transferGroups.values()) {
        if (!group.transferIDs.some((id) => removedIDs.has(id))) continue;
        group.transferIDs = group.transferIDs.filter((id) => !removedIDs.has(id));
        const remaining = group.transferIDs.map((id) => state.transferByID.get(id)).filter(Boolean);
        group.totalSize = remaining.reduce((total, transfer) => total + transferFileSize(transfer), 0);
        group.transferSummary = aggregateTransferSummary(remaining);
        if (!remaining.some((transfer) => transfer.state === "failed")) group.error = "";
        updateTransferGroup(group.id, false);
      }
      announce(`${failed.length} failed uploads cleared.`);
    } catch (error) {
      if (state.transferLedgerOwner !== ownerID) return;
      for (const transfer of failed) { transfer.cleared = false; queueTransferPersistence(transfer); }
      for (const groupID of removedGroups) {
        const group = state.transferGroups.get(groupID);
        if (group) { group.cleared = false; persistTransferGroup(group); }
      }
      showToast(`Failed uploads could not be cleared. ${friendlyError(error, "Try again.")}`, "error");
    } finally {
      for (const transfer of failed) transfer.clearing = false;
      rebuildTransferProjection();
      renderTransfers();
      setTransferSheetOpen(!byID("transfer-panel").hidden);
    }
  }

  function updateTransferGroup(groupID, notify = true) {
    if (!groupID) return;
    const group = state.transferGroups.get(groupID);
    if (!group || group.state === "preparing") return;
    const summary = group.transferSummary || aggregateTransferSummary(group.transferIDs.map((id) => state.transferByID.get(id)).filter(Boolean));
    group.transferSummary = summary;
    if (group.cancelled && summary.counts.complete !== summary.totalCount) group.state = "cancelled";
    else if (group.discoveryDone !== false && summary.totalCount > 0 && summary.counts.complete === summary.totalCount) group.state = "complete";
    else if (summary.counts.preparing + summary.counts.uploading + summary.counts["retry-wait"] + summary.counts.paused > 0) group.state = "uploading";
    else if (summary.counts.queued + summary.counts["needs-source"] > 0) group.state = "queued";
    else if (summary.counts.failed > 0) group.state = "failed";
    else if (group.discoveryDone === false) group.state = "queued";
    else group.state = "cancelled";
    scheduleTransferGroupPersistence(group);
    if (group.state === "complete" && !group.refreshed) {
      group.refreshed = true;
      queueUploadDirectoryRefresh(group.baseDirectory);
      if (notify) announce(`${group.name} uploaded with ${summary.totalCount} files.`);
    }
    if (group.state === "failed" && !group.failureAnnounced) {
      group.failureAnnounced = true;
      if (notify) announce(`${group.name} has ${summary.counts.failed} failed uploads.`, true);
    }
  }

  async function cancelTransferGroup(group) {
    group.cancelled = true;
    group.state = "cancelled";
    group.directoryPreparationController?.abort();
    state.transferPlanControllers.get(group.id)?.abort();
    window.clearTimeout(state.uploadPlanRetryTimers.get(group.id));
    state.uploadPlanRetryTimers.delete(group.id);
    const cancelled = state.transfers.filter((item) => item.groupID === group.id && ["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(item.state));
	const cancellableUploads = [];
    for (const transfer of cancelled) {
      transfer.cancelRequested = true;
      transfer.directoryPreparationController?.abort();
      cancelUploadFingerprint(transfer);
      if (transfer.controller) transfer.controller.abort();
      transfer.attemptGeneration = (transfer.attemptGeneration || 0) + 1;
      transfer.pendingAction = transfer.uploadID ? "cancel" : "";
	  if (transfer.uploadID) cancellableUploads.push(transfer);
      window.clearTimeout(state.transferRetryTimers.get(transfer.id));
      state.transferRetryTimers.delete(transfer.id);
      transitionTransfer(transfer, "cancelled", "Cancelled", "cancelled");
    }
    await persistTransferItems(cancelled);
    await persistTransferGroup(group);
	if (!group.fixture && cancellableUploads.length) {
	  const grouped = new Map();
	  for (const transfer of cancellableUploads) {
		const key = transfer.batchID || "legacy";
		if (!grouped.has(key)) grouped.set(key, []);
		grouped.get(key).push(transfer);
	  }
	  for (const [index, [batchID, members]] of [...grouped.entries()].entries()) {
		const transfers = members.filter((transfer) => transfer.pendingAction === "cancel" && !transfer.cleared);
		if (!transfers.length) continue;
		const owner = state.user?.userID;
		const generations = transfers.map((transfer) => transfer.attemptGeneration);
		const current = () => transfers.filter((transfer, offset) => owner === state.user?.userID && generations[offset] === transfer.attemptGeneration && transfer.pendingAction === "cancel" && !transfer.cleared);
		const body = { uploadIDs: transfers.map((transfer) => transfer.uploadID) };
		const count = transfers[0]?.batchCount || 0;
		const indices = new Set(transfers.map((transfer) => transfer.batchIndex));
		const completeBatch = batchID !== "legacy" && count === transfers.length && indices.size === count
		  && transfers.every((transfer) => transfer.batchID === batchID && transfer.batchCount === count && transfer.batchIndex >= 0 && transfer.batchIndex < count);
		if (completeBatch) body.batchID = batchID;
		try { await uploadNamespaceMutation("/api/v1/uploads/batch", { method: "DELETE", headers: { "Idempotency-Key": `${group.id}-cancel-${index}` }, body }); for (const transfer of current()) { transfer.pendingAction = ""; queueTransferPersistence(transfer); } }
        catch { const pending = current(); if (pending.length) await retryPendingCancellation(pending); }
	  }
	}
    rebuildTransferProjection();
    renderTransfers();
    byID("transfer-live").textContent = `${cancelled.length} transfers cancelled.`;
  }

  function renewTransferIdentity(transfer) {
    const priorID = transfer.id;
    transfer.identityNeedsPersistence = true;
    transfer.durableAdmissionRequired = true;
    transfer.replacedIdentityIDs = [...(transfer.replacedIdentityIDs || []),priorID];
    transfer.id = idempotencyKey();
    state.transferByID.delete(priorID);
    state.transferByID.set(transfer.id, transfer);
    if (state.transferOverviewIDs.delete(priorID)) state.transferOverviewIDs.add(transfer.id);
    const group = state.transferGroups.get(transfer.groupID);
    if (group) {
      if (!group.transferIndex || group.transferIndex.size !== group.transferIDs.length) group.transferIndex = new Map(group.transferIDs.map((id, index) => [id, index]));
      const index = group.transferIndex.get(priorID);
      if (index !== undefined) { group.transferIDs[index] = transfer.id; group.transferIndex.delete(priorID); group.transferIndex.set(transfer.id, index); }
      scheduleTransferGroupPersistence(group);
    }
    if (transfer.sourceHandle) persistTransferSource(transfer, transfer.sourceHandle);
  }

  function restartExpiredTransfer(transfer) {
    renewTransferIdentity(transfer);
    restoreTransferProgress(transfer, 0);
    transfer.uploadID = "";
    transfer.batchID = ""; transfer.batchIndex = 0; transfer.batchCount = 0;
    transfer.admissionBatchKey = ""; transfer.admissionBatchIndex = 0; transfer.admissionBatchCount = 0;
    transfer.admissionProtocol = "independent"; transfer.admissionIntent = null; transfer.pendingAdmissionStatus = null;
    transfer.completionBatchKey = ""; transfer.completionBatchIndex = -1;
    transfer.completionProtocol = ""; transfer.pendingAction = "";
    transfer.awaitingCompletion = false; transfer.pendingCapability = null;
    transfer.sourceExpectedMD5 = ""; transfer.sourceExpectedCRC32C = "";
    resetUploadPlanForReconnectedSource(transfer);
    setPreparationState(transfer, transfer.planPhase !== "done" ? "duplicates" : "sessions");
    queueTransferPersistence(transfer);
    transfer.identityPersistence = persistTransferIdentity(transfer);
  }

  let transferIdentityQueue = [];
  let transferIdentityScheduled = false;
  function persistTransferIdentity(transfer) {
    return new Promise((resolve) => {
      transferIdentityQueue.push({ transfer, resolve });
      if (transferIdentityScheduled) return;
      transferIdentityScheduled = true;
      queueMicrotask(async () => {
        const pending = transferIdentityQueue; transferIdentityQueue = []; transferIdentityScheduled = false;
        const saved = await persistTransferItems(pending.map((item) => item.transfer),true);
        for (const item of pending) item.resolve(saved);
      });
    });
  }

  async function waitForTransferIdentity(transfer) {
    if (!transfer.identityNeedsPersistence) return;
    const id = transfer.id;
    const saved = await (transfer.identityPersistence || persistTransferIdentity(transfer));
    transfer.identityPersistence = null;
    if (id !== transfer.id || transfer.cancelRequested || transfer.suspendedByLogout) throw new DOMException("Upload attempt replaced", "AbortError");
    if (!saved) {
      throw uploadIdentityPersistenceError();
    }
    transfer.identityNeedsPersistence = false;
    transfer.replacedIdentityIDs = [];
  }

  async function retryPendingCancellation(transfers) {
    const owner = state.user?.userID;
    for (const transfer of transfers) { preserveTransferError(transfer); transfer.pendingAction = "cancel"; transfer.cancelRequested = true; }
    try { await applyUploadActions(transfers, "cancel"); }
    catch (error) {
      if (owner !== state.user?.userID || error.name === "AbortError") return;
      for (const transfer of transfers) transitionTransfer(transfer, "failed", "Cancellation could not be confirmed. Retry cancellation.", error instanceof APIError ? error.code : "unavailable");
    }
    if (owner === state.user?.userID) { rebuildTransferProjection(); renderTransfers(); }
  }

  function freshAdmissionRequiredForRetry(transfer) {
    return transfer.admissionProtocol === "independent" && !transfer.uploadID && transfer.admissionIntent
      && ["conflict", "precondition_failed"].includes(transfer.errorCode);
  }

  function beginTransferRetry(transfer) {
    const restart = transfer.state === "cancelled" || ["expired", "aborted"].includes(transfer.errorCode) || freshAdmissionRequiredForRetry(transfer);
    beginTransferAttempt(transfer, true);
    if (restart) restartExpiredTransfer(transfer);
    return restart;
  }

  function retryTransferGroup(group) {
    if (group.cleared || group.transferIDs.some((id) => state.transferByID.get(id)?.clearing)) return;
    group.cancelled = false;
    group.refreshed = false;
    group.failureAnnounced = false;
    const cancelling = [];
    for (const transfer of state.transfers.filter((item) => item.groupID === group.id && ["failed", "cancelled"].includes(item.state))) {
      if (transfer.pendingAction === "cancel") { cancelling.push(transfer); continue; }
	  if (transfer.awaitingCompletion && transfer.uploadID && transfer.crc32c) {
        beginTransferAttempt(transfer);
        setPreparationState(transfer, "ready", true);
		transfer.cancelRequested = false;
		transfer.state = "uploading";
		transfer.error = "";
		transfer.errorCode = "";
		queueTransferPersistence(transfer);
		continue;
	  }
      beginTransferRetry(transfer);
      transfer.cancelRequested = false;
      transfer.state = transfer.planPhase && transfer.planPhase !== "done" ? "preparing" : "queued";
      transfer.error = "";
      transfer.speedBps = 0;
      transfer.lastProgressAt = 0;
      transfer.lastProgressBytes = transfer.confirmed;
      transfer.recoveryFailures = 0;
      transfer.retryCount = 0;
      transfer.nextRetryAt = 0;
      queueTransferPersistence(transfer);
    }
    if (cancelling.length) retryPendingCancellation(cancelling);
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
    group.state = state.transfers.some((item) => item.groupID === group.id && item.planPhase && item.planPhase !== "done") ? "preparing" : "queued";
    persistTransferGroup(group);
    rebuildTransferProjection();
    renderTransfers();
    resumeUploadPlans();
    pumpTransfers();
  }

  function retryTransfer(transfer) {
    if (transfer.clearing || transfer.cleared) return;
    if (transfer.pendingAction === "cancel") { retryPendingCancellation([transfer]); return; }
	if (transfer.awaitingCompletion && transfer.uploadID && transfer.crc32c) {
      beginTransferAttempt(transfer);
      setPreparationState(transfer, "ready", true);
	  transfer.cancelRequested = false;
	  transfer.state = "uploading";
	  transfer.error = "";
	  transfer.errorCode = "";
	  queueTransferPersistence(transfer);
	  rebuildTransferProjection();
	  renderTransfers();
	  flushUploadCompletions();
	  return;
	}
    beginTransferRetry(transfer);
    transfer.cancelRequested = false;
    transfer.state = transfer.fixture ? "uploading" : transfer.planPhase && transfer.planPhase !== "done" ? "preparing" : "queued";
    transfer.error = "";
    transfer.speedBps = transfer.fixture ? 4 * (1 << 20) : 0;
    transfer.lastProgressAt = 0;
    transfer.lastProgressBytes = transfer.confirmed;
    transfer.recoveryFailures = 0;
    transfer.retryCount = 0;
    transfer.nextRetryAt = 0;
    queueTransferPersistence(transfer);
    if (!transfer.fixture) {
      resumeUploadPlans();
      pumpTransfers();
    }
    rebuildTransferProjection();
    renderTransfers();
  }

  function retryFailedTransfers() {
    const failed = state.transfers.filter((transfer) => transfer.state === "failed" && !transfer.clearing && !transfer.cleared && transferCanRetry(transfer));
    const failedGroupIDs = new Set(failed.map((transfer) => transfer.groupID).filter(Boolean));
    const cancelling = [];
    for (const transfer of failed) {
      if (transfer.pendingAction === "cancel") { cancelling.push(transfer); continue; }
	  if (transfer.awaitingCompletion && transfer.uploadID && transfer.crc32c) {
        beginTransferAttempt(transfer);
        setPreparationState(transfer, "ready", true);
		transfer.retryCount = 0;
		transfer.nextRetryAt = 0;
		transfer.cancelRequested = false;
		transitionTransfer(transfer, "uploading");
		continue;
	  }
      beginTransferRetry(transfer);
      transfer.retryCount = 0;
      transfer.nextRetryAt = 0;
      transfer.cancelRequested = false;
      transitionTransfer(transfer, transfer.fixture ? "uploading" : transfer.planPhase && transfer.planPhase !== "done" ? "preparing" : "queued");
      if (transfer.fixture) transfer.speedBps = 4 * (1 << 20);
    }
    if (cancelling.length) retryPendingCancellation(cancelling);
    for (const group of state.transferGroups.values()) {
      if (failedGroupIDs.has(group.id)) {
        group.cancelled = false;
        group.failureAnnounced = false;
        group.state = failed.some((transfer) => transfer.groupID === group.id && transfer.planPhase && transfer.planPhase !== "done") ? "preparing" : "queued";
        persistTransferGroup(group);
      }
    }
    rebuildTransferProjection();
    renderTransfers();
    resumeUploadPlans();
    pumpTransfers();
    showToast(failed.length ? `${failed.length} failed transfers returned to the queue.` : "No failed transfers can be retried without reconnecting their source.", failed.length ? "success" : "info");
  }

  async function reconnectTransferSources(files) {
    const groupID = state.transferReconnectGroup;
    state.transferReconnectGroup = null;
    const candidates = state.transfers.filter((transfer) => transfer.groupID === groupID && !transfer.clearing && transferNeedsSource(transfer));
    const supplied = new Map(Array.from(files).filter((file) => file instanceof File).map((file) => [file.webkitRelativePath || file.name, file]));
    let connected = 0;
    for (const transfer of candidates) {
      const file = supplied.get(transfer.relativePath) || supplied.get(transfer.name);
      if (!file || file.size !== transferFileSize(transfer) || (transfer.lastModified && file.lastModified !== transfer.lastModified)) continue;
      transfer.file = file;
      if (!beginTransferRetry(transfer)) resetUploadPlanForReconnectedSource(transfer);
      transitionTransfer(transfer, transfer.planPhase && transfer.planPhase !== "done" ? "preparing" : "queued");
      connected += 1;
    }
    rebuildTransferProjection();
    renderTransfers();
    resumeUploadPlans();
    pumpTransfers();
    showToast(`${connected} of ${candidates.length} transfer sources reconnected.`, connected === candidates.length ? "success" : "warning");
  }

  async function reconnectStoredTransferSources(groupID) {
    const candidates = state.transfers.filter((transfer) => transfer.groupID === groupID && !transfer.clearing && transferNeedsSource(transfer));
    let connected = 0;
    for (const transfer of candidates) {
      const handle = transfer.sourceHandle;
      if (!handle || typeof handle.requestPermission !== "function" || typeof handle.getFile !== "function") continue;
      try {
        if (await handle.requestPermission({ mode: "read" }) !== "granted") continue;
        const file = await handle.getFile();
        if (file.size !== transferFileSize(transfer) || (transfer.lastModified && file.lastModified !== transfer.lastModified)) continue;
        transfer.file = file;
        if (!beginTransferRetry(transfer)) resetUploadPlanForReconnectedSource(transfer);
        transitionTransfer(transfer, transfer.planPhase && transfer.planPhase !== "done" ? "preparing" : "queued");
        connected += 1;
      } catch {
        // Explicit file selection below remains available when a stored handle
        // cannot be reopened or no longer identifies the original source.
      }
    }
    const remaining = candidates.filter(transferNeedsSource);
    if (connected) {
      renderTransfers();
      resumeUploadPlans();
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
    let checkedBytes = 0;
    let planning = 0;
    let preparationUnits = 0, preparedUnits = 0;
    const preparationPhases = {};
    const cohorts = new Set();
    let fixtures = false;
    for (const transfer of transfers) {
      if (Object.hasOwn(counts, transfer.state)) counts[transfer.state] += 1;
      const size = transferFileSize(transfer);
      totalBytes += size;
      preparationUnits += preparationWeight(transfer);
      preparedUnits += preparationCredit(transfer);
      const phase = preparationPhase(transfer);
      if (phase) preparationPhases[phase] = (preparationPhases[phase] || 0) + 1;
      checkedBytes += uploadCheckBytes(transfer);
      if (transfer.strategy && transfer.strategy !== "keep-both") planning++;
      if (transfer.fixture) fixtures = true;
      else cohorts.add(uploadCohort(transfer));
      confirmedBytes += Math.min(transfer.confirmed, size);
      if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(transfer.state)) remainingBytes += Math.max(0, size - transfer.confirmed);
      if (transfer.state === "uploading" && Number.isFinite(transfer.speedBps)) speedBps += transfer.speedBps;
      if (transfer.state === "failed" && transferCanRetry(transfer)) retryableFailed += 1;
    }
    if (!fixtures) speedBps = measuredUploadRate(cohorts, uploadMeasurementNow(), Math.min(remainingBytes || totalBytes, uploadSourceChunkBytes));
    return finalizeTransferSummary({ counts, totalCount: transfers.length, totalBytes, confirmedBytes, remainingBytes, speedBps, retryableFailed, checkedBytes, planning, preparationUnits, preparedUnits, preparationPhases, estimateKey: [...cohorts].sort().join(":") });
  }

  function finalizeTransferSummary(summary) {
    summary.preparationPercent = summary.preparationUnits ? Math.floor(Math.min(summary.preparationUnits, summary.preparedUnits || 0) / summary.preparationUnits * 100) : 0;
    summary.retryableFailed = Number.isSafeInteger(summary.retryableFailed) ? summary.retryableFailed : 0;
    summary.percent = summary.totalBytes > 0
      ? Math.round(summary.confirmedBytes / summary.totalBytes * 100)
      : summary.totalCount > 0 ? Math.round(summary.counts.complete / summary.totalCount * 100) : 0;
    summary.active = summary.counts.preparing + summary.counts.uploading;
    summary.pending = summary.counts.queued + summary.counts["retry-wait"] + summary.counts.paused + summary.counts["needs-source"];
    let eta = "ETA —";
    if (summary.totalCount > 0 && summary.counts.complete === summary.totalCount) eta = "Complete";
    else if (!navigator.onLine && summary.active + summary.pending > 0) eta = "Offline";
    else if (summary.counts.uploading > 0 && summary.remainingBytes === 0 && summary.counts.preparing === 0 && summary.pending === 0) eta = "Finalizing";
    else if (summary.speedBps > 0 && summary.remainingBytes > 0) {
      const now = uploadMeasurementNow();
      let estimate = uploadEstimates.get(summary.estimateKey);
      if (!estimate || now - estimate.at >= 5000) {
        estimate = { at: now, seconds: summary.remainingBytes / summary.speedBps };
        uploadEstimates.set(summary.estimateKey, estimate);
      }
      eta = formatDuration(estimate.seconds);
    }
    else if (summary.active > 0) eta = summary.counts.uploading ? "Measuring speed" : "Preparing uploads";
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
    // Include complete and failed members of each ongoing upload, but keep
    // unrelated terminal history out of its progress denominator. With no
    // ongoing work, retain the latest upload's result across ledger reloads.
    const ongoing = new Set();
    let latest = "";
    let latestCreated = -1;
    for (const transfer of state.transfers) {
      const cohort = transfer.groupID || transfer.id;
      const created = state.transferGroups.get(transfer.groupID)?.createdAt || transfer.createdAt || 0;
      if (created > latestCreated || (created === latestCreated && cohort > latest)) { latest = cohort; latestCreated = created; }
      if (!["complete", "failed", "cancelled"].includes(transfer.state)) ongoing.add(cohort);
    }
    if (!ongoing.size && latest) ongoing.add(latest);
    const current = state.transfers.filter((transfer) => ongoing.has(transfer.groupID || transfer.id));
    state.transferOverviewIDs = new Set(current.map((transfer) => transfer.id));
    state.transferOverview = aggregateTransferSummary(current);
  }

  function updateAggregateProgress(summary, transfer, priorConfirmed, priorSpeed) {
    if (!summary) return;
    const size = transferFileSize(transfer);
    const confirmedDelta = Math.min(transfer.confirmed, size) - Math.min(priorConfirmed, size);
    summary.confirmedBytes += confirmedDelta;
    if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(transfer.state)) summary.remainingBytes = Math.max(0, summary.remainingBytes - confirmedDelta);
    if (transfer.fixture && transfer.state === "uploading") summary.speedBps = Math.max(0, summary.speedBps + transfer.speedBps - priorSpeed);
    finalizeTransferSummary(summary);
  }

  function updateAggregateState(summary, transfer, priorState, nextState, priorSpeed) {
    if (!summary || priorState === nextState) return;
    const size = transferFileSize(transfer);
    const remaining = Math.max(0, size - transfer.confirmed);
    changePreparationSummary(summary, preparationCredit(transfer, priorState), preparationCredit(transfer, nextState), preparationPhase(transfer, priorState), preparationPhase(transfer, nextState));
    if (Object.hasOwn(summary.counts, priorState)) summary.counts[priorState] = Math.max(0, summary.counts[priorState] - 1);
    if (Object.hasOwn(summary.counts, nextState)) summary.counts[nextState] += 1;
    if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(priorState)) summary.remainingBytes = Math.max(0, summary.remainingBytes - remaining);
    if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(nextState)) summary.remainingBytes += remaining;
    if (transfer.fixture && priorState === "uploading") summary.speedBps = Math.max(0, summary.speedBps - priorSpeed);
    if (transfer.fixture && nextState === "uploading") summary.speedBps += transfer.speedBps;
    if (priorState === "failed" && transferCanRetry(transfer)) summary.retryableFailed = Math.max(0, summary.retryableFailed - 1);
    if (nextState === "failed" && transferCanRetry(transfer)) summary.retryableFailed += 1;
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
    const recentSpeed = !transfer.fixture && uploadMeasurementNow() - transfer.lastProgressAt >= 15000 ? 0 : transfer.speedBps;
    if (transfer.state === "uploading") eta = transfer.awaitingCompletion ? (transfer.retryCount > 0 ? "Retrying finalization" : "Finalizing") : transfer.sourcePhase === "reading" ? "Reading source" : transfer.sourcePhase === "waiting-source" ? "Waiting for source read" : recentSpeed > 0 ? "Uploading" : "Waiting for acknowledgement";
    else if (transfer.state === "preparing") eta = preparationFeedback({ preparationPhases: { [preparationPhase(transfer)]: 1 } }) || "Preparing";
    else if (transfer.state === "retry-wait") eta = transfer.nextRetryAt > Date.now() ? `Retry in ${formatDuration((transfer.nextRetryAt - Date.now()) / 1000)}` : "Retrying";
    else if (transfer.state === "paused") eta = "Offline";
    else if (transfer.state === "needs-source") eta = "Source needed";
    else if (transfer.state === "complete") eta = "Complete";
    else if (transfer.state === "cancelled") eta = "Cancelled";
    return `${percent}% · ${eta} · ${formatRate(recentSpeed)}`;
  }

  function stackedTransferProgress(progress, checked, maximum, label, enabled) {
    const stack = document.createElement("div");
    stack.className = "transfer-progress-stack";
    const checking = document.createElement("progress");
    checking.className = "transfer-check-progress";
    checking.max = Math.max(1, maximum);
    checking.value = checked;
    checking.hidden = !enabled;
    checking.setAttribute("aria-label", `Upload preparation progress for ${label}`);
    // Keep upload progress first for consumers of the existing semantic row.
    stack.append(progress, checking);
    return stack;
  }

  function transferRow(transfer, sharedFailure = false) {
    const row = document.createElement("div");
    row.className = `transfer-row ${transfer.state}`;
    const label = transfer.groupID ? transfer.relativePath : transfer.name;
    const tail = document.createElement("div");
    tail.className = "transfer-row-tail";
    if (transferNeedsSource(transfer)) tail.append(iconButton("upload", `Reconnect upload source for ${label}`, () => openTransferReconnect(transfer.groupID || ""), "transfer-row-actions", "Reconnect source"));
    if (["queued", "preparing", "uploading", "retry-wait", "paused", "needs-source"].includes(transfer.state)) tail.append(iconButton("x", `Cancel upload ${label}`, () => cancelTransfer(transfer), "transfer-row-actions", "Cancel upload"));
    if ((transfer.state === "failed" || (!transfer.groupID && transfer.state === "cancelled")) && transferCanRetry(transfer)) {
      tail.append(iconButton("refresh", `Retry upload ${label}`, () => retryTransfer(transfer), "transfer-row-actions", "Retry upload"));
    }
    if (transfer.state === "failed") {
      const clear = iconButton("trash", `Clear failed upload ${label}`, () => clearFailedTransfers([transfer]), "transfer-row-actions", "Clear failed upload");
      clear.disabled = Boolean(transfer.clearing);
      tail.append(clear);
    }
    const header = document.createElement("div");
    header.className = "transfer-row-header";
    header.append(text("strong", label, "transfer-row-main"), tail);
    row.dataset.transferId = transfer.id;
    if (transfer.state === "failed") {
      const details = document.createElement("details");
      details.className = "transfer-failure-details";
      details.open = state.expandedTransferFailures.get(transfer.id) ?? !sharedFailure;
      const disclosure = document.createElement("summary");
      disclosure.setAttribute("aria-label", `Failure details for ${label}`);
      const name = text("strong", label, "transfer-row-main");
      name.title = label;
      disclosure.append(name, text("span", "Failed", "transfer-failure-state"), applicationIcon("chevron-down"));
      details.append(disclosure, text("span", transfer.error || "Upload failed.", "transfer-row-error"));
      if (transfer.errorCode) details.append(text("span", transfer.errorCode, "transfer-failure-code"));
      details.append(text("span", `${formatBytes(transfer.confirmed)} of ${formatBytes(transferFileSize(transfer))}`, "transfer-failure-bytes"));
      details.addEventListener("toggle", () => {
        if (details.isConnected) state.expandedTransferFailures.set(transfer.id, details.open);
      });
      row.append(details, tail);
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
    row.append(header, metrics, stackedTransferProgress(progress, preparationCredit(transfer), preparationWeight(transfer), label, true));
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

  function transferFailureKey(transfer) {
    // The message includes the request reference. Never coalesce different
    // references or codes just because their human-readable prefix agrees.
    return JSON.stringify([transfer.errorCode || "", transfer.error || "Upload failed."]);
  }

  function sharedTransferFailures(transfers) {
    const failures = new Map();
    for (const transfer of transfers) {
      if (transfer.state !== "failed") continue;
      const key = transferFailureKey(transfer);
      const failure = failures.get(key);
      if (failure) failure.count += 1;
      else failures.set(key, { message: transfer.error || "Upload failed.", count: 1 });
    }
    return new Map([...failures].filter(([, failure]) => failure.count > 1).sort(([leftKey, left], [rightKey, right]) => right.count - left.count || (leftKey < rightKey ? -1 : leftKey > rightKey ? 1 : 0)));
  }

  function transferGroupRow(group, transfers, sharedFailures) {
    const summary = group.transferSummary || aggregateTransferSummary(transfers);
    const displayState = groupDisplayState(group, transfers);
    const row = document.createElement("div");
    row.className = `transfer-group-row ${displayState}`;
    const tail = document.createElement("div");
    tail.className = "transfer-row-tail";
    if (transfers.some(transferNeedsSource)) tail.append(iconButton("folder-up", `Reconnect upload source for ${group.name}`, () => openTransferReconnect(group.id), "transfer-row-actions", "Reconnect source"));
    if (["preparing", "queued", "uploading", "retry-wait", "paused"].includes(group.state)) tail.append(iconButton("x", `Cancel folder upload ${group.name}`, () => cancelTransferGroup(group), "transfer-row-actions", "Cancel folder upload"));
    if (["failed", "cancelled"].includes(group.state)) tail.append(iconButton("refresh", `Retry folder upload ${group.name}`, () => retryTransferGroup(group), "transfer-row-actions", "Retry folder upload"));
    if (summary.counts.failed) {
      const clear = iconButton("trash", `Clear failed uploads in folder ${group.name}`, () => clearFailedTransfers(transfers), "transfer-row-actions", "Clear failed uploads");
      clear.disabled = transfers.some((transfer) => transfer.clearing);
      tail.append(clear);
    }
    const expanded = state.expandedTransferGroups.has(group.id);
    const disclosure = iconButton(expanded ? "chevron-up" : "chevron-down", `${expanded ? "Collapse" : "Expand"} upload group ${group.name}`, () => {
      if (expanded) state.expandedTransferGroups.delete(group.id);
      else state.expandedTransferGroups.add(group.id);
      rebuildTransferProjection();
      renderTransferWindow();
    }, "transfer-row-actions", expanded ? "Collapse group" : "Expand group");
    disclosure.setAttribute("aria-expanded", String(expanded));
    disclosure.dataset.transferAction = "disclosure";
    tail.append(disclosure);
    const header = document.createElement("div");
    header.className = "transfer-row-header";
    header.append(text("strong", group.name, "transfer-row-main"), tail);
    const metrics = document.createElement("div");
    metrics.className = "transfer-row-metrics";
    const metricText = summary.preparationPercent < 100 && !summary.counts.uploading
      ? `Preparing ${summary.preparationPercent}% · ${preparationFeedback(summary)}`
      : group.state === "preparing" && !transfers.some((transfer) => transfer.state !== "queued")
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
    row.append(header, metrics, stackedTransferProgress(progress, summary.preparedUnits || 0, summary.preparationUnits, `folder ${group.name}`, true));
    if (group.error && ![...sharedFailures.values()].some((failure) => failure.message === group.error)) row.append(text("span", group.error, "transfer-row-error"));
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
    const history = summary;
    summary = state.transferOverview || summary;
    const progress = byID("transfer-progress");
    progress.max = summary.totalBytes > 0 ? summary.totalBytes : Math.max(1, summary.totalCount);
    progress.value = summary.totalBytes > 0 ? summary.confirmedBytes : summary.counts.complete;
    progress.setAttribute("aria-valuetext", `${summary.percent}% uploaded`);
    const checking = byID("transfer-check-progress");
    checking.max = Math.max(1, summary.preparationUnits || 0);
    checking.value = summary.preparedUnits || 0;
    checking.hidden = false;
    checking.setAttribute("aria-valuetext", `${summary.preparationPercent}% prepared. ${preparationFeedback(summary)}`);
    byID("transfer-check-label").textContent = `Preparing ${summary.preparationPercent}%`;
    byID("transfer-check-label").title = preparationFeedback(summary) || "All upload prerequisites are ready.";
    byID("transfer-check-label").parentElement.dataset.checking = "active";
    byID("transfer-upload-label").textContent = `Uploaded ${summary.percent}%`;
    byID("transfer-percent").textContent = `${summary.percent}%`;
    const eta = summary.eta.replace(" remaining", "");
    byID("transfer-eta").textContent = /^\d/.test(eta) ? `ETA ${eta}` : eta === "Calculating ETA" ? "ETA …" : eta;
    byID("transfer-speed").textContent = formatRate(summary.speedBps);
    byID("transfer-volume").textContent = `${formatBytes(summary.confirmedBytes)} / ${formatBytes(summary.totalBytes)}`;
    byID("transfer-active").textContent = `${summary.active.toLocaleString()} active`;
    const filterCounts = {
      current: history.active + history.pending + history.counts.failed,
      failed: history.counts.failed,
      complete: history.counts.complete,
      all: history.totalCount,
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
    const earlierFailures = Math.max(0, history.counts.failed - summary.counts.failed);
    const launcherLabel = `View transfers, ${summary.percent}% complete, ${formatRate(summary.speedBps)}, ${summary.eta}, ${summary.active} active of ${summary.totalCount} files${earlierFailures ? `, ${earlierFailures} earlier failed uploads` : ""}`;
    byID("open-transfers").setAttribute("aria-label", launcherLabel);
    byID("open-transfers").dataset.tooltip = launcherLabel;
    byID("clear-transfers").disabled = history.counts.complete + history.counts.cancelled === 0;
    byID("retry-failed-transfers").disabled = history.retryableFailed === 0;
    byID("clear-failed-transfers").disabled = !state.transfers.some((transfer) => transfer.state === "failed" && !transfer.clearing);
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
      const matchingFiles = transfers.filter((transfer) => transferMatchesFilter(transfer, filter) && transferMatchesSearch(transfer));
      const sharedFailures = sharedTransferFailures(matchingFiles);
      projection.push({ kind: "group", group, transfers, sharedFailures });
      for (const [key, failure] of sharedFailures) projection.push({ kind: "failure", failure, key: JSON.stringify([group.id, key]), nested: true });
      if (!state.expandedTransferGroups.has(group.id)) continue;
      const filteredFiles = orderedTransfers(matchingFiles);
      for (const transfer of filteredFiles) {
        projection.push({ kind: "transfer", transfer, nested: true, sharedFailure: sharedFailures.has(transferFailureKey(transfer)) });
      }
    }
    state.transferProjection = projection;
    state.transferVirtualStart = Math.min(state.transferVirtualStart, Math.max(0, projection.length - transferVirtualWindowSize));
  }

  function renderTransferWindow(preserveContext = true) {
    const list = byID("transfer-list");
    const listTop = list.getBoundingClientRect().top;
    const anchor = preserveContext ? [...list.children].find((item) => item.dataset.transferViewKey && item.getBoundingClientRect().bottom > listTop) : null;
    const anchorKey = anchor?.dataset.transferViewKey;
    const anchorTop = anchor?.getBoundingClientRect().top;
    const focused = list.contains(document.activeElement) ? document.activeElement : null;
    const focusedRow = focused?.closest("[data-transfer-id], [data-transfer-group-id]");
    const focusID = focusedRow?.dataset.transferId || focusedRow?.dataset.transferGroupId;
    const focusLabel = focused?.getAttribute("aria-label");
    const focusAction = focused?.dataset.transferAction;
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
      item.dataset.transferViewKey = JSON.stringify([view.kind, view.kind === "group" ? view.group.id : view.kind === "transfer" ? view.transfer.id : view.key]);
      item.setAttribute("aria-posinset", String(start + offset + 1));
      item.setAttribute("aria-setsize", String(state.transferProjection.length));
      if (view.kind === "transfer" && view.transfer.state === "failed") item.classList.add("failed-transfer-item");
      if (view.kind === "failure") {
        item.classList.add("shared-failure-item");
        item.append(text("span", `${view.failure.count.toLocaleString()} files · ${view.failure.message}`, "transfer-row-error transfer-shared-error"));
      } else item.append(view.kind === "group" ? transferGroupRow(view.group, view.transfers, view.sharedFailures) : transferRow(view.transfer, view.sharedFailure));
      list.append(item);
    }
    list.setAttribute("aria-label", `Transfer items ${start + 1} to ${end} of ${state.transferProjection.length}`);
    if (anchorKey) {
      const nextAnchor = [...list.children].find((item) => item.dataset.transferViewKey === anchorKey);
      if (nextAnchor) list.scrollTop = Math.max(0, list.scrollTop + nextAnchor.getBoundingClientRect().top - anchorTop);
    }
    if (focusID) {
      const nextRow = [...list.querySelectorAll("[data-transfer-id], [data-transfer-group-id]")].find((row) => (row.dataset.transferId || row.dataset.transferGroupId) === focusID);
      const nextFocus = nextRow && [...nextRow.querySelectorAll("button, summary")].find((control) => focusAction ? control.dataset.transferAction === focusAction : control.getAttribute("aria-label") === focusLabel);
      if (nextFocus) nextFocus.focus({ preventScroll: true });
    }
  }

  function shiftTransferWindow(direction) {
    const list = byID("transfer-list");
    const priorStart = state.transferVirtualStart;
    const maximumStart = Math.max(0, state.transferProjection.length - transferVirtualWindowSize);
    state.transferVirtualStart = Math.max(0, Math.min(maximumStart, priorStart + direction * transferVirtualWindowStep));
    const moved = state.transferVirtualStart - priorStart;
    if (!moved) return;
    // The overlapping item anchors both windows. Error text can change row
    // heights, so preserve its measured viewport position rather than assuming
    // a fixed height for every removed or inserted row.
    const anchorPosition = Math.max(priorStart, state.transferVirtualStart) + 1;
    const anchorSelector = `[aria-posinset="${anchorPosition}"]`;
    const priorAnchor = list.querySelector(anchorSelector);
    const priorTop = priorAnchor?.getBoundingClientRect().top;
    renderTransferWindow(false);
    const nextAnchor = list.querySelector(anchorSelector);
    if (priorTop !== undefined && nextAnchor) list.scrollTop = Math.max(1, list.scrollTop + nextAnchor.getBoundingClientRect().top - priorTop);
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
      const checking = row.querySelector(".transfer-check-progress");
      const metric = row.querySelector(".transfer-row-metric");
      const bytes = row.querySelector(".transfer-row-bytes");
      if (row.dataset.transferId) {
        const transfer = state.transferByID.get(row.dataset.transferId);
        if (!transfer) continue;
        if (transfer.state === "failed") {
          const error = row.querySelector(".transfer-row-error");
          if (error) error.textContent = transfer.error || "Upload failed.";
          const confirmed = row.querySelector(".transfer-failure-bytes");
          if (confirmed) confirmed.textContent = `${formatBytes(transfer.confirmed)} of ${formatBytes(transferFileSize(transfer))}`;
          continue;
        }
        const size = transferFileSize(transfer);
        metric.textContent = transferMetricText(transfer);
        bytes.textContent = `${formatBytes(transfer.confirmed)} of ${formatBytes(size)}`;
        progress.max = Math.max(1, size);
        progress.value = size === 0 && transfer.state === "complete" ? 1 : transfer.confirmed;
        if (checking) { checking.max = preparationWeight(transfer); checking.value = preparationCredit(transfer); }
        continue;
      }
      const group = state.transferGroups.get(row.dataset.transferGroupId);
      const summary = group && group.transferSummary;
      if (!group || !summary) continue;
      metric.textContent = summary.preparationPercent < 100 && !summary.counts.uploading
        ? `Preparing ${summary.preparationPercent}% · ${preparationFeedback(summary)}`
        : `${summary.percent}% · ${summary.eta} · ${formatRate(summary.speedBps)}`;
      bytes.textContent = `${summary.counts.complete} of ${summary.totalCount} files${summary.counts.failed ? ` · ${summary.counts.failed} failed` : ""}`;
      progress.max = group.totalSize > 0 ? group.totalSize : Math.max(1, summary.totalCount);
      progress.value = group.totalSize > 0 ? summary.confirmedBytes : summary.totalCount ? summary.counts.complete : group.state === "complete" ? 1 : 0;
      if (checking) { checking.max = Math.max(1, summary.preparationUnits || 0); checking.value = summary.preparedUnits || 0; }
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
      preparedDirectories: new Set(),
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
