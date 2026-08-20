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
