  async function copyMove(move, entries = [...state.selected.values()], clearSelection = true) {
    const result = await ask({ title: `${move ? "Move" : "Copy"} ${entries.length} item${entries.length === 1 ? "" : "s"}`, description: "Enter a destination folder path. Names are kept unchanged.", confirm: move ? "Move" : "Copy", fields: [{ id: "destination", label: "Destination folder", value: state.currentDirectory, required: true }], conflictField: true });
    if (!result) return;
    const items = entries.map((entry) => ({ source: entry.path, destination: joinPath(result.destination.replace(/\/$/, "") || "/", entry.name), conflict: result.conflict }));
    try {
      const endpoint = move ? "/api/v1/files/move" : "/api/v1/files/copy";
      const operation = await api(endpoint, { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: items.length === 1 ? items[0] : { items } });
      announce(`${move ? "Move" : "Copy"} started for ${items.length} item${items.length === 1 ? "" : "s"}.`);
      if (clearSelection) state.selected.clear();
      else if (move) for (const entry of entries) state.selected.delete(entrySelectionKey(entry));
      updateSelection();
      await awaitOperation(operation);
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
      else for (const entry of entries) state.selected.delete(entrySelectionKey(entry));
      updateSelection();
      await awaitOperation(result);
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

  async function awaitOperation(result) {
    if (!result) return;
    const operation = result.operation || result;
    if (operation.state === "succeeded") return;
    if (operation.state === "failed") throw new Error(operation.error || "Operation failed.");
    if (Array.isArray(result.items)) {
      if (!result.items.length || result.items.every((item) => item.state === "succeeded")) return;
      const failed = result.items.find((item) => item.state === "failed");
      if (failed) throw new Error(failed.error || "Operation failed.");
    }
    await watchOperation(result.operationID || operation.id);
  }

  async function loadTrash(append = false) {
    if (append && state.trashLoading) return;
    const request = state.trashRequest + 1;
    state.trashRequest = request;
    state.trashLoading = true;
    if (!append) {
      cleanupGridMedia(new Set());
      state.previewStates.clear();
      state.currentEntry = null;
      renderPathAggregate(null);
      state.entries = [];
      state.trashCursor = "";
      state.selected.clear();
      updateSelection();
      byID("list-presentation").scrollTop = 0;
      byID("media-grid").scrollTop = 0;
      showState("drive-state", "");
      renderFileLoadingItems();
    }
    try {
      const cursor = append && state.trashCursor ? `&cursor=${encodeURIComponent(state.trashCursor)}` : "";
      const page = await api(`/api/v1/trash?limit=${browserPageSize}${cursor}`);
      if (request !== state.trashRequest || state.browserAccess !== "trash") return;
      const entries = (page.items || []).map(trashBrowserEntry);
      state.entries = append ? state.entries.concat(entries) : entries;
      state.trashCursor = page.nextCursor || "";
      renderFiles();
      syncBrowserURLState("replace", "trash");
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
      if (request === state.trashRequest) {
        state.trashLoading = false;
        requestAnimationFrame(() => maybeLoadNextBrowserPage(state.viewMode === "grid" ? byID("media-grid") : byID("list-presentation")));
      }
    }
  }

  function trashBrowserEntry(item) {
    return {
      path: item.originalPath,
      name: item.originalPath,
      kind: item.kind,
      size: item.size,
      fileCount: item.fileCount,
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
      await awaitOperation(operation); announce(`${item.originalPath} restored.`); await loadTrash();
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function deleteTrash(item) {
    const confirmed = await ask({ title: `Permanently delete ${item.originalPath}?`, description: "This cannot be undone. The file data will be permanently removed.", confirm: "Delete Permanently", danger: true });
    if (!confirmed) return;
    try {
      const operation = await api(`/api/v1/trash/${encodeURIComponent(item.trashID)}`, { method: "DELETE", headers: { "Idempotency-Key": idempotencyKey() }, body: {} });
      await awaitOperation(operation); announce(`${item.originalPath} permanently deleted.`); await loadTrash();
    } catch (error) { announce(friendlyError(error), true); }
  }

  async function runTrashSelection(entries, operation, actionLabel, completedLabel) {
    const selectionBar = byID("selection-bar");
    selectionBar.setAttribute("aria-busy", "true");
    selectionBar.inert = true;
    try {
      showToast(`${actionLabel} ${entries.length} selected item${entries.length === 1 ? "" : "s"}…`, "info", 30000);
      const result = await operation(entries.map((entry) => entry.trash.trashID));
      await awaitOperation(result);
      state.selected.clear();
      updateSelection();
      await loadTrash();
      showToast(`${entries.length} item${entries.length === 1 ? "" : "s"} ${completedLabel}.`, "success");
    } catch (error) {
      showActionErrorToast(actionLabel, error, "The selected items were not changed.");
    } finally {
      selectionBar.removeAttribute("aria-busy");
      selectionBar.inert = false;
    }
  }

  async function restoreSelectedTrash() {
    const entries = [...state.selected.values()];
    if (!entries.length) return;
    const result = await ask({ title: `Restore ${entries.length} selected item${entries.length === 1 ? "" : "s"}?`, description: "Selected items return to their original locations.", confirm: "Restore", conflictField: true });
    if (!result) return;
    await runTrashSelection(entries, (trashIDs) => api("/api/v1/trash/restore", {
      method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: { trashIDs, conflict: result.conflict },
    }), "Restore", "restored");
  }

  async function deleteSelectedTrash() {
    const entries = [...state.selected.values()];
    if (!entries.length) return;
    const confirmed = await ask({ title: `Permanently delete ${entries.length} selected item${entries.length === 1 ? "" : "s"}?`, description: "The selected items and their file data will be removed. This cannot be undone.", confirm: "Delete Permanently", danger: true });
    if (!confirmed) return;
    await runTrashSelection(entries, (trashIDs) => api("/api/v1/trash/delete", {
      method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: { trashIDs },
    }), "Permanent delete", "permanently deleted");
  }

  async function emptyTrash() {
    const confirmed = await ask({ title: "Permanently delete every trashed item?", description: "All items in Trash will be removed. This cannot be undone.", confirm: "Empty Trash Permanently", danger: true });
    if (!confirmed) return;
    try {
      const result = await api("/api/v1/trash/empty", { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body: { confirm: true } });
      await awaitOperation(result);
      announce("Trash emptied."); await loadTrash();
    } catch (error) { announce(friendlyError(error), true); }
  }
