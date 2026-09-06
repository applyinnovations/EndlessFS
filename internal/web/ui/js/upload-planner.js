  const uploadPlanningBatchSize = 10000;
  const uploadHashWorkerLimit = 2;

  async function chooseUploadStrategy(label = "these items") {
    if (state.config && state.config.localFixture) return "keep-both";
    const result = await ask({
      title: "Upload options",
      description: `Choose how EndlessFS should handle content that already exists while uploading ${label}. Files with unique sizes can start immediately while ambiguous files are checked locally.`,
      confirm: "Start upload",
      choiceField: {
        id: "upload-strategy", label: "Duplicate strategy", options: [
          ["smart-merge", "Smart merge — reuse identical content and keep changed files"],
          ["replace-changed", "Replace changed files — reuse identical content"],
          ["only-new", "Only add new names — leave existing paths unchanged"],
          ["keep-both", "Keep both — upload every file with a unique name"],
        ],
      },
    });
    return result ? result["upload-strategy"] : "";
  }

  function uploadHashWorker() {
    let worker = state.uploadHashWorkers.find((candidate) => !candidate.busy);
    if (!worker && state.uploadHashWorkers.length < uploadHashWorkerLimit) {
      worker = new Worker("/assets/upload-hash-worker.js");
      worker.busy = false;
      worker.addEventListener("message", (event) => {
        const pending = state.uploadHashPending.get(event.data && event.data.id);
        if (!pending || pending.worker !== worker) return;
        state.uploadHashPending.delete(event.data.id);
        worker.busy = false;
        if (event.data.error) pending.reject(new Error(event.data.error));
        else pending.resolve({ md5: event.data.md5, crc32c: event.data.crc32c });
        dispatchUploadHashQueue();
      });
      worker.addEventListener("error", (event) => {
        for (const [id, pending] of state.uploadHashPending) {
          if (pending.worker !== worker) continue;
          state.uploadHashPending.delete(id);
          pending.reject(new Error(event.message || "Local fingerprinting failed."));
        }
        worker.terminate();
        state.uploadHashWorkers = state.uploadHashWorkers.filter((candidate) => candidate !== worker);
        dispatchUploadHashQueue();
      });
      state.uploadHashWorkers.push(worker);
    }
    return worker;
  }

  function dispatchUploadHashQueue() {
    while (state.uploadHashQueue.length) {
      const worker = uploadHashWorker();
      if (!worker || worker.busy) return;
      const job = state.uploadHashQueue.shift();
      if (job.transfer.cancelRequested) {
        job.reject(new DOMException("Cancelled", "AbortError"));
        continue;
      }
      worker.busy = true;
      state.uploadHashPending.set(job.transfer.id, { ...job, worker });
      worker.postMessage({ type: "hash", id: job.transfer.id, file: job.transfer.file });
    }
  }

  function fingerprintUploadFile(transfer) {
    return new Promise((resolve, reject) => {
      state.uploadHashQueue.push({ transfer, resolve, reject });
      dispatchUploadHashQueue();
    });
  }

  function cancelUploadFingerprint(transfer) {
    state.uploadHashQueue = state.uploadHashQueue.filter((job) => {
      if (job.transfer !== transfer) return true;
      job.reject(new DOMException("Cancelled", "AbortError"));
      return false;
    });
    const pending = state.uploadHashPending.get(transfer.id);
    if (!pending) return;
    state.uploadHashPending.delete(transfer.id);
    pending.worker.terminate();
    state.uploadHashWorkers = state.uploadHashWorkers.filter((candidate) => candidate !== pending.worker);
    pending.reject(new DOMException("Cancelled", "AbortError"));
    dispatchUploadHashQueue();
  }

  function uploadPlanPath(transfer) {
    return joinPath(transfer.directory, transfer.name);
  }

  function markPlannedUpload(transfer, conflict = "rename", expectedVersion = "") {
    transfer.planPhase = "done";
    transfer.planOutcome = "upload";
    transfer.uploadConflict = conflict;
    transfer.targetVersion = expectedVersion;
    transitionTransfer(transfer, "queued");
  }

  function markPlannedComplete(transfer, outcome) {
    transfer.planPhase = "done";
    transfer.planOutcome = outcome;
    transfer.confirmed = transferFileSize(transfer);
    transitionTransfer(transfer, "complete");
    queueUploadDirectoryRefresh(transfer.directory);
  }

  function applyUnmatchedUploadDecision(transfer) {
    if (!transfer.targetExists) {
      markPlannedUpload(transfer, "fail");
      return;
    }
    if (transfer.strategy === "only-new") {
      markPlannedComplete(transfer, "skipped-existing-name");
    } else if (transfer.strategy === "replace-changed" && transfer.targetKind === "file") {
      markPlannedUpload(transfer, "replace", transfer.targetVersion);
    } else {
      markPlannedUpload(transfer, "rename");
    }
  }

  async function planUploadSizeBatch(transfers, signal) {
    const response = await api("/api/v1/uploads/plan/sizes", {
      method: "POST", signal,
      body: { items: transfers.map((transfer) => ({ id: transfer.id, path: uploadPlanPath(transfer), size: transferFileSize(transfer) })) },
    });
    if (!response || !response.token || !Array.isArray(response.items) || response.items.length !== transfers.length) throw new Error("Upload size planning returned an invalid response.");
    const byID = new Map(response.items.map((item) => [item.id, item]));
    const exact = [];
    for (const transfer of transfers) {
      if (transfer.cancelRequested) continue;
      const item = byID.get(transfer.id);
      if (!item) throw new Error("Upload size planning omitted an item.");
      transfer.targetExists = Boolean(item.targetExists);
      transfer.targetKind = item.targetKind || "";
      transfer.targetVersion = item.targetVersion || "";
      if (!item.fingerprintRequired) {
        applyUnmatchedUploadDecision(transfer);
        continue;
      }
      transfer.planPhase = transfer.md5 && transfer.crc32c ? "exact-pending" : "hash-pending";
      transitionTransfer(transfer, "preparing", transfer.planPhase === "hash-pending" ? "Checking content locally." : "Checking for matching content.");
      exact.push(transfer);
    }
    await Promise.all(transfers.map(persistTransferItem));
    pumpTransfers();
    return { token: response.token, exact };
  }

  async function hashUploadCandidates(transfers) {
    await Promise.all(transfers.map(async (transfer) => {
      if (transfer.cancelRequested) return;
      if (transfer.md5 && transfer.crc32c) return;
      transfer.planPhase = "hashing";
      transitionTransfer(transfer, "preparing", "Checking content locally.");
      await persistTransferItem(transfer);
      let fingerprint;
      try {
        fingerprint = await fingerprintUploadFile(transfer);
      } catch (error) {
        if (transfer.cancelRequested || error && error.name === "AbortError") return;
        throw error;
      }
      if (transfer.cancelRequested) return;
      transfer.md5 = fingerprint.md5;
      transfer.crc32c = fingerprint.crc32c;
      transfer.planPhase = "exact-pending";
      transitionTransfer(transfer, "preparing", "Checking for matching content.");
      await persistTransferItem(transfer);
    }));
  }

  async function applyUploadFingerprintDecisions(token, transfers, group, signal) {
    const active = transfers.filter((transfer) => !transfer.cancelRequested);
    if (!active.length) return;
    const response = await api("/api/v1/uploads/plan/fingerprints", {
      method: "POST", signal,
      body: { token, items: active.map((transfer) => ({
        id: transfer.id, path: uploadPlanPath(transfer), size: transferFileSize(transfer), md5: transfer.md5, crc32c: transfer.crc32c,
      })) },
    });
    if (!response || !Array.isArray(response.items) || response.items.length !== active.length) throw new Error("Upload fingerprint planning returned an invalid response.");
    const byID = new Map(response.items.map((item) => [item.id, item]));
    const reuse = [];
    for (const transfer of active) {
      if (transfer.cancelRequested) continue;
      const item = byID.get(transfer.id);
      if (!item) throw new Error("Upload fingerprint planning omitted an item.");
      transfer.targetExists = Boolean(item.targetExists);
      transfer.targetKind = item.targetKind || "";
      transfer.targetVersion = item.targetVersion || "";
      if (item.action === "skip") {
        markPlannedComplete(transfer, "skipped-identical-target");
      } else if (item.action === "reuse" && item.sourcePath && item.sourceVersion && !(transfer.strategy === "only-new" && transfer.targetExists)) {
        let conflict = "fail";
        if (transfer.targetExists) conflict = transfer.strategy === "replace-changed" && transfer.targetKind === "file" ? "replace" : "rename";
        reuse.push({ transfer, source: item.sourcePath, sourceVersion: item.sourceVersion, conflict });
      } else {
        applyUnmatchedUploadDecision(transfer);
      }
    }
    const activeReuse = reuse.filter(({ transfer }) => !transfer.cancelRequested);
    if (activeReuse.length) {
      await Promise.all(activeReuse.map(({ transfer }) => ensureTransferDirectories(transfer)));
      // The first stable transfer ID makes each 1000-item planning batch
      // independently replayable even when one folder spans several batches.
      const reuseKey = `${activeReuse[0].transfer.id}-content-reuse`;
      await api("/api/v1/files/copy", {
        method: "POST", signal, headers: { "Idempotency-Key": reuseKey }, body: { items: activeReuse.map(({ transfer, source, sourceVersion, conflict }) => ({
          source, destination: uploadPlanPath(transfer), conflict, expectedSource: sourceVersion,
          ...(conflict === "replace" ? { expectedTarget: transfer.targetVersion } : {}),
        })) },
      });
      for (const { transfer } of activeReuse) markPlannedComplete(transfer, "reused-content");
    }
    await Promise.all(active.map(persistTransferItem));
  }

  async function planUploadTransfers(transfers, group = null) {
    const pending = transfers.filter((transfer) => transfer && transfer.planPhase && transfer.planPhase !== "done" && !transfer.cancelRequested);
    if (!pending.length || !navigator.onLine) return;
    const key = group ? group.id : pending.map((transfer) => transfer.id).sort().join(":");
    if (state.transferPlanControllers.has(key)) return;
    window.clearTimeout(state.uploadPlanRetryTimers.get(key));
    state.uploadPlanRetryTimers.delete(key);
    const controller = new AbortController();
    state.transferPlanControllers.set(key, controller);
    if (group) { group.state = "preparing"; group.error = ""; persistTransferGroup(group); }
    try {
      for (let offset = 0; offset < pending.length; offset += uploadPlanningBatchSize) {
        const batch = pending.slice(offset, offset + uploadPlanningBatchSize);
        const planned = await planUploadSizeBatch(batch, controller.signal);
        await hashUploadCandidates(planned.exact);
        await applyUploadFingerprintDecisions(planned.token, planned.exact, group, controller.signal);
      }
      for (const transfer of pending) { transfer.retryCount = 0; transfer.nextRetryAt = 0; }
      if (group && !group.cancelled) await prepareTransferGroup(group);
      rebuildTransferProjection();
      renderTransfers();
      pumpTransfers();
      const reused = pending.filter((transfer) => ["reused-content", "skipped-identical-target"].includes(transfer.planOutcome)).length;
      const skipped = pending.filter((transfer) => transfer.planOutcome === "skipped-existing-name").length;
      if (reused || skipped) showToast(`${reused} identical ${reused === 1 ? "file" : "files"} reused or skipped${skipped ? `; ${skipped} existing ${skipped === 1 ? "name" : "names"} left unchanged` : ""}.`, "success");
    } catch (error) {
      const suspended = pending.some((transfer) => transfer.suspendedByLogout);
      const staleSnapshot = error instanceof APIError && error.status === 409;
      let retryable = !navigator.onLine || staleSnapshot || transferFailureIsRetryable(error) || suspended;
      const retryCandidates = pending.filter((item) => item.planPhase !== "done" && !item.cancelRequested);
      for (const transfer of retryCandidates) {
        transfer.retryCount = (transfer.retryCount || 0) + 1;
        if (transfer.retryCount > transferRetryLimit) retryable = false;
        if (staleSnapshot) transfer.planPhase = "size-pending";
      }
      const scheduleRetry = retryCandidates.length > 0 && retryable && navigator.onLine && !suspended && !(group && group.cancelled);
      const retryDelay = scheduleRetry ? Math.max(...retryCandidates.map(transferRetryDelay)) : 0;
      for (const transfer of pending.filter((item) => item.planPhase !== "done")) {
        if (transfer.cancelRequested || transfer.state === "cancelled") continue;
        transfer.nextRetryAt = scheduleRetry ? Date.now() + retryDelay : 0;
        const waiting = navigator.onLine ? "Duplicate check will retry automatically." : "Duplicate check will resume when connected.";
        transitionTransfer(transfer, retryable ? (navigator.onLine ? "retry-wait" : "paused") : "failed", retryable ? waiting : friendlyError(error, "Duplicate checking failed."), retryable ? (navigator.onLine ? "transient" : "offline") : "planning_failed");
      }
      if (group && !group.cancelled) { group.state = retryable ? (navigator.onLine ? "retry-wait" : "paused") : "failed"; group.error = retryable ? "Duplicate check will retry automatically." : friendlyError(error, "Duplicate checking failed."); persistTransferGroup(group); }
      await Promise.all(pending.map(persistTransferItem));
      renderTransfers();
      if (scheduleRetry) {
        state.uploadPlanRetryTimers.set(key, window.setTimeout(() => {
          state.uploadPlanRetryTimers.delete(key);
          for (const transfer of pending) {
            if (["paused", "retry-wait"].includes(transfer.state) && transfer.planPhase !== "done" && !transfer.cancelRequested) transitionTransfer(transfer, "preparing");
          }
          planUploadTransfers(pending, group);
        }, retryDelay));
      }
    } finally {
      if (state.transferPlanControllers.get(key) === controller) state.transferPlanControllers.delete(key);
    }
  }

  function resumeUploadPlans() {
    if (!navigator.onLine) return;
    const grouped = new Map();
    for (const transfer of state.transfers) {
      if (!transfer.planPhase || transfer.planPhase === "done" || !(transfer.file instanceof File) || transfer.cancelRequested) continue;
      const key = transfer.groupID || transfer.id;
      if (!grouped.has(key)) grouped.set(key, []);
      grouped.get(key).push(transfer);
    }
    for (const [key, transfers] of grouped) planUploadTransfers(transfers, state.transferGroups.get(key) || null);
  }

  function installUploadPlannerTestFixture() {
    if (!state.config || !state.config.localFixture) return;
    window.__endlessfsUploadPlannerTest = {
      fingerprint: async (value) => fingerprintUploadFile({ id: idempotencyKey(), file: new File([value], "fixture.bin"), cancelRequested: false }),
      fingerprintRepeated: async (value, count) => fingerprintUploadFile({ id: idempotencyKey(), file: new File([value.repeat(count)], "fixture-large.bin"), cancelRequested: false }),
      workerLimit: uploadHashWorkerLimit,
      batchSize: uploadPlanningBatchSize,
    };
  }
