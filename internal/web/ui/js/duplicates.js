  const duplicateGroupPageSize = 30;
  const duplicateOccurrencePageSize = 100;
  const duplicateOverlapPageSize = 30;

  function duplicateLocation(value) {
    return { area: value.area || "live", path: value.path };
  }

  function duplicateOccurrenceKey(value) {
    return `${value.area || "live"}:${value.path}`;
  }

  function duplicateKindLabel(kind, count = 1) {
    if (kind === "directory") return count === 1 ? "folder" : "folders";
    return count === 1 ? "file" : "files";
  }

  function duplicatePathName(path) {
    const parts = String(path || "").split("/").filter(Boolean);
    return parts[parts.length - 1] || "/";
  }

  function duplicateURLState() {
    const query = new URLSearchParams(location.search);
    const kinds = query.getAll("kind");
    const folders = query.getAll("folder");
    const kind = kinds.length === 1 ? kinds[0] : "";
    const folder = folders.length === 1 ? folders[0] : "";
    return {
      kind: kind === "file" || kind === "directory" ? kind : "directory",
      folder: folder.startsWith("/") && folder.length <= 4096 ? folder : "",
    };
  }

  function syncDuplicateURL(push = false) {
    if (location.pathname !== "/duplicates") return;
    const query = new URLSearchParams();
    query.set("kind", state.duplicateKind);
    if (state.duplicateFolderPath) query.set("folder", state.duplicateFolderPath);
    if (state.safeTheme) query.set("safe-theme", "1");
    const target = `/duplicates?${query.toString()}`;
    if (`${location.pathname}${location.search}` === target) return;
    history[push ? "pushState" : "replaceState"]({ route: "duplicates" }, "", target);
  }

  async function loadDuplicates() {
    const urlState = duplicateURLState();
    state.duplicateKind = urlState.kind;
    state.duplicateFolderPath = urlState.folder;
    byID(`duplicate-kind-${state.duplicateKind}`).checked = true;
    byID("duplicate-folder-path").value = state.duplicateFolderPath;
    syncDuplicateURL(false);
    const loads = [loadDuplicateGroups(false)];
    if (state.duplicateFolderPath) loads.push(loadDuplicateOverlaps(false));
    else clearDuplicateOverlaps();
    await Promise.all(loads);
  }

  async function refreshDuplicates() {
    const control = byID("duplicate-refresh");
    control.disabled = true;
    try {
      const loads = [loadDuplicateGroups(false)];
      if (state.duplicateFolderPath) loads.push(loadDuplicateOverlaps(false));
      await Promise.all(loads);
      announce("Duplicate results refreshed.");
    } finally {
      control.disabled = false;
    }
  }

  function renderDuplicateGroupLoading() {
    const target = byID("duplicate-groups");
    const fragment = document.createDocumentFragment();
    for (let index = 0; index < 3; index += 1) {
      const card = document.createElement("article");
      card.className = "duplicate-card loading-item-row";
      const header = document.createElement("div");
      header.className = "duplicate-card-header";
      header.setAttribute("aria-hidden", "true");
      header.append(document.createElement("span"), document.createElement("span"));
      card.append(header);
      fragment.append(card);
    }
    target.replaceChildren(fragment);
    target.setAttribute("aria-busy", "true");
  }

  async function loadDuplicateGroups(append = false) {
    if (append && state.duplicateGroupsLoading) return;
    const request = state.duplicateGroupsRequest + 1;
    state.duplicateGroupsRequest = request;
    state.duplicateGroupsLoading = true;
    if (!append) {
      state.duplicateGroups = [];
      state.duplicateGroupsCursor = "";
      state.duplicateOccurrences.clear();
      showState("duplicate-groups-state", "");
      renderDuplicateGroupLoading();
    }
    byID("duplicate-groups-more").disabled = true;
    try {
      const query = new URLSearchParams({
        kind: state.duplicateKind,
        limit: String(duplicateGroupPageSize),
        includeIgnored: "true",
      });
      if (append && state.duplicateGroupsCursor) query.set("cursor", state.duplicateGroupsCursor);
      const page = await api(`/api/v1/duplicates/groups?${query.toString()}`);
      if (request !== state.duplicateGroupsRequest) return;
      state.duplicateGroups = append ? state.duplicateGroups.concat(page.groups || []) : (page.groups || []);
      state.duplicateGroupsCursor = page.nextCursor || "";
      renderDuplicateGroups();
    } catch (error) {
      if (request !== state.duplicateGroupsRequest) return;
      if (!append) {
        state.duplicateGroups = [];
        renderDuplicateGroups();
        showState("duplicate-groups-state", friendlyError(error, "Exact copies could not be loaded."), "error");
      } else announce(friendlyError(error, "More exact copies could not be loaded."), true);
    } finally {
      if (request === state.duplicateGroupsRequest) {
        state.duplicateGroupsLoading = false;
        byID("duplicate-groups").removeAttribute("aria-busy");
        byID("duplicate-groups-more").disabled = false;
      }
    }
  }

  function renderDuplicateGroups() {
    const active = state.duplicateGroups.filter((group) => !group.ignored);
    const ignored = state.duplicateGroups.filter((group) => group.ignored);
    renderDuplicateGroupCollection("duplicate-groups", active, false);
    renderDuplicateGroupCollection("duplicate-ignored-groups", ignored, true);
    const summary = byID("duplicate-summary");
    summary.replaceChildren();
    if (active.length) {
      summary.append(text("strong", active.length), text("span", `active ${duplicateKindLabel(state.duplicateKind)} group${active.length === 1 ? "" : "s"} loaded`));
    }
    const ignoredDisclosure = byID("duplicate-ignored-disclosure");
    ignoredDisclosure.hidden = ignored.length === 0;
    byID("duplicate-ignored-count").textContent = String(ignored.length);
    byID("duplicate-groups-more").hidden = !state.duplicateGroupsCursor;
    if (!state.duplicateGroupsLoading && state.duplicateGroups.length === 0) {
      showState("duplicate-groups-state", `No exact duplicate ${duplicateKindLabel(state.duplicateKind, 2)} were found.`);
    } else if (state.duplicateGroups.length) showState("duplicate-groups-state", "");
  }

  function renderDuplicateGroupCollection(targetID, groups, ignored) {
    const target = byID(targetID);
    const fragment = document.createDocumentFragment();
    for (const group of groups) fragment.append(duplicateGroupCard(group, ignored));
    target.replaceChildren(fragment);
  }

  function duplicateGroupCard(group, ignored) {
    const card = document.createElement("article");
    card.className = "duplicate-card";
    card.dataset.groupID = group.id;
    const header = document.createElement("div");
    header.className = "duplicate-card-header";
    const description = document.createElement("div");
    description.className = "duplicate-card-description";
    const title = document.createElement("div");
    title.className = "duplicate-card-title";
    title.append(text("h3", `${group.occurrenceCount} identical ${duplicateKindLabel(group.kind, group.occurrenceCount)}`), text("span", "Exact copy · 100%", "duplicate-badge exact"));
    const meta = document.createElement("div");
    meta.className = "duplicate-card-meta";
    meta.append(
      text("span", `${formatBytes(group.size)} each`),
      text("span", `${group.fileCount} ${duplicateKindLabel("file", group.fileCount)}`),
      text("span", `Up to ${formatBytes(group.reclaimableBytes)} reclaimable in this group`),
    );
    description.append(title, meta);
    const actions = document.createElement("div");
    actions.className = "duplicate-card-actions";
    const occurrenceState = state.duplicateOccurrences.get(group.id);
    const expanded = Boolean(occurrenceState && occurrenceState.expanded);
    actions.append(
      button(expanded ? "Hide locations" : "Review locations", () => toggleDuplicateGroup(group), "quiet"),
      button(ignored ? "Unignore" : "Ignore", () => setDuplicateGroupIgnored(group, !ignored), "quiet"),
    );
    header.append(description, actions);
    card.append(header);
    if (expanded) card.append(duplicateGroupBody(group, occurrenceState));
    return card;
  }

  function duplicateGroupBody(group, occurrenceState) {
    const body = document.createElement("div");
    body.className = "duplicate-card-body";
    if (occurrenceState.loading && !occurrenceState.items.length) {
      body.append(text("p", "Loading locations…", "muted"));
      return body;
    }
    const list = document.createElement("ul");
    list.className = "duplicate-occurrences";
    const live = occurrenceState.items.filter((item) => item.area === "live");
    if (!live.some((item) => duplicateOccurrenceKey(item) === occurrenceState.keepKey) && live.length) occurrenceState.keepKey = duplicateOccurrenceKey(live[0]);
    for (const occurrence of occurrenceState.items) {
      const key = duplicateOccurrenceKey(occurrence);
      const item = document.createElement("li");
      item.className = "duplicate-occurrence";
      const label = document.createElement("label");
      const keep = document.createElement("input");
      keep.type = "radio";
      keep.name = `duplicate-keep-${group.id}`;
      keep.value = key;
      keep.checked = key === occurrenceState.keepKey;
      keep.disabled = occurrence.area !== "live";
      keep.addEventListener("change", () => { occurrenceState.keepKey = key; renderDuplicateGroups(); });
      label.append(keep, text("span", occurrence.path, "duplicate-occurrence-path"));
      const area = text("span", occurrence.area === "live" ? (keep.checked ? "Keep" : "Copy") : "Already in Trash", "duplicate-occurrence-area");
      item.append(label, area);
      if (group.kind === "directory" && occurrence.area === "live") {
        item.append(button("Find overlaps", () => inspectDuplicateFolder(occurrence.path), "quiet"));
      }
      list.append(item);
    }
    body.append(list);
    const footer = document.createElement("div");
    footer.className = "duplicate-card-footer";
    const actionCount = Math.max(0, live.length - 1);
    footer.append(text("p", occurrenceState.nextCursor ? "Showing a bounded page of locations." : "Choose the copy to keep; every other loaded live copy can move to Trash."));
    if (occurrenceState.nextCursor) footer.append(button("Load more locations", () => loadDuplicateOccurrences(group, true), "quiet"));
    const cleanup = button(`Move ${actionCount} ${duplicateKindLabel(group.kind, actionCount)} to Trash`, () => trashExactDuplicates(group), "danger");
    cleanup.disabled = actionCount < 1 || occurrenceState.loading;
    footer.append(cleanup);
    body.append(footer);
    return body;
  }

  async function toggleDuplicateGroup(group) {
    let occurrenceState = state.duplicateOccurrences.get(group.id);
    if (!occurrenceState) {
      occurrenceState = { items: [], nextCursor: "", loading: false, expanded: true, keepKey: "" };
      state.duplicateOccurrences.set(group.id, occurrenceState);
      renderDuplicateGroups();
      await loadDuplicateOccurrences(group, false);
      return;
    }
    occurrenceState.expanded = !occurrenceState.expanded;
    renderDuplicateGroups();
    if (occurrenceState.expanded && occurrenceState.items.length === 0) await loadDuplicateOccurrences(group, false);
  }

  async function loadDuplicateOccurrences(group, append) {
    const occurrenceState = state.duplicateOccurrences.get(group.id);
    if (!occurrenceState || occurrenceState.loading) return;
    occurrenceState.loading = true;
    renderDuplicateGroups();
    try {
      let endpoint = `/api/v1/duplicates/groups/${encodeURIComponent(group.id)}/occurrences?limit=${duplicateOccurrencePageSize}`;
      if (append && occurrenceState.nextCursor) endpoint += `&cursor=${encodeURIComponent(occurrenceState.nextCursor)}`;
      const page = await api(endpoint);
      occurrenceState.items = append ? occurrenceState.items.concat(page.occurrences || []) : (page.occurrences || []);
      occurrenceState.nextCursor = page.nextCursor || "";
      const firstLive = occurrenceState.items.find((item) => item.area === "live");
      if (!occurrenceState.keepKey && firstLive) occurrenceState.keepKey = duplicateOccurrenceKey(firstLive);
    } catch (error) {
      announce(friendlyError(error, "Duplicate locations could not be loaded."), true);
    } finally {
      occurrenceState.loading = false;
      renderDuplicateGroups();
    }
  }

  async function setDuplicateGroupIgnored(group, ignored) {
    try {
      await api(`/api/v1/duplicates/groups/${encodeURIComponent(group.id)}/ignore`, {
        method: "PUT",
        body: { ignored, expectedRevision: group.ignoreRevision || 0 },
      });
      announce(ignored ? "Exact copies ignored." : "Exact copies returned to active results.");
      await loadDuplicateGroups(false);
    } catch (error) {
      announce(friendlyError(error, "The duplicate ignore setting could not be saved."), true);
    }
  }

  async function trashExactDuplicates(group) {
    const occurrenceState = state.duplicateOccurrences.get(group.id);
    if (!occurrenceState) return;
    const live = occurrenceState.items.filter((item) => item.area === "live");
    const keep = live.find((item) => duplicateOccurrenceKey(item) === occurrenceState.keepKey);
    const remove = live.filter((item) => duplicateOccurrenceKey(item) !== occurrenceState.keepKey).slice(0, 100);
    if (!keep || !remove.length) return;
    const confirmed = await ask({
      title: `Move ${remove.length} duplicate ${duplicateKindLabel(group.kind, remove.length)} to Trash?`,
      description: `${keep.path} will stay in Files. This action is recoverable from Trash.`,
      confirm: "Move to Trash",
      danger: true,
    });
    if (!confirmed) return;
    try {
      const result = await api("/api/v1/files/trash", {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: { items: remove.map((item) => ({ path: item.path, version: item.version })) },
      });
      if (result.operationID) await watchOperation(result.operationID);
      const failed = (result.items || []).filter((item) => item.state !== "succeeded").length;
      if (failed) showToast(`${remove.length - failed} copies moved to Trash; ${failed} changed before cleanup.`, "warning", 12000);
      else showTrashUndo(result.items || []);
      state.duplicateOccurrences.delete(group.id);
      await loadDuplicateGroups(false);
      if (state.duplicateFolderPath) await loadDuplicateOverlaps(false);
      byID("duplicate-exact-title").focus();
    } catch (error) {
      announce(friendlyError(error, "Duplicate copies could not be moved to Trash."), true);
    }
  }

  function clearDuplicateOverlaps() {
    state.duplicateOverlapsRequest += 1;
    state.duplicateOverlaps = [];
    state.duplicateOverlapsCursor = "";
    byID("duplicate-overlaps").replaceChildren();
    byID("duplicate-ignored-overlaps").replaceChildren();
    byID("duplicate-overlap-summary").replaceChildren();
    byID("duplicate-overlaps-more").hidden = true;
    byID("duplicate-ignored-overlaps-disclosure").hidden = true;
    showState("duplicate-overlaps-state", "Enter a folder path to find exact, subset, and partial matches.");
  }

  function inspectDuplicateFolder(path) {
    state.duplicateFolderPath = path;
    byID("duplicate-folder-path").value = path;
    syncDuplicateURL(true);
    loadDuplicateOverlaps(false);
    byID("duplicate-overlap-title").focus({ preventScroll: true });
    const behavior = matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth";
    byID("duplicate-overlap-title").scrollIntoView({ behavior, block: "start" });
  }

  async function submitDuplicateFolder(event) {
    event.preventDefault();
    const value = byID("duplicate-folder-path").value.trim();
    if (!value.startsWith("/") || value === "/") {
      showState("duplicate-overlaps-state", "Enter a non-root folder path beginning with /.", "error");
      return;
    }
    state.duplicateFolderPath = value;
    syncDuplicateURL(true);
    await loadDuplicateOverlaps(false);
  }

  async function loadDuplicateOverlaps(append = false) {
    if (!state.duplicateFolderPath || (append && state.duplicateOverlapsLoading)) return;
    const request = state.duplicateOverlapsRequest + 1;
    state.duplicateOverlapsRequest = request;
    state.duplicateOverlapsLoading = true;
    if (!append) {
      state.duplicateOverlaps = [];
      state.duplicateOverlapsCursor = "";
      showState("duplicate-overlaps-state", "Finding folder matches…");
      byID("duplicate-overlaps").replaceChildren();
      byID("duplicate-ignored-overlaps").replaceChildren();
    }
    byID("duplicate-overlaps-more").disabled = true;
    try {
      const page = await api("/api/v1/duplicates/directories/overlaps", {
        method: "POST",
        body: {
          directory: { area: "live", path: state.duplicateFolderPath },
          limit: duplicateOverlapPageSize,
          cursor: append ? state.duplicateOverlapsCursor : "",
          includeIgnored: true,
        },
      });
      if (request !== state.duplicateOverlapsRequest) return;
      state.duplicateOverlaps = append ? state.duplicateOverlaps.concat(page.candidates || []) : (page.candidates || []);
      state.duplicateOverlapsCursor = page.nextCursor || "";
      renderDuplicateOverlaps();
    } catch (error) {
      if (request !== state.duplicateOverlapsRequest) return;
      if (!append) {
        state.duplicateOverlaps = [];
        renderDuplicateOverlaps();
        showState("duplicate-overlaps-state", friendlyError(error, "Folder matches could not be loaded."), "error");
      } else announce(friendlyError(error, "More folder matches could not be loaded."), true);
    } finally {
      if (request === state.duplicateOverlapsRequest) {
        state.duplicateOverlapsLoading = false;
        byID("duplicate-overlaps-more").disabled = false;
      }
    }
  }

  function renderDuplicateOverlaps() {
    const active = state.duplicateOverlaps.filter((candidate) => !candidate.ignored && !candidate.exactGroupIgnored).sort(compareDuplicateCandidates);
    const ignored = state.duplicateOverlaps.filter((candidate) => candidate.ignored || candidate.exactGroupIgnored).sort(compareDuplicateCandidates);
    renderDuplicateOverlapCollection("duplicate-overlaps", active, false);
    renderDuplicateOverlapCollection("duplicate-ignored-overlaps", ignored, true);
    const summary = byID("duplicate-overlap-summary");
    summary.replaceChildren();
    if (active.length) summary.append(text("strong", active.length), text("span", `active match${active.length === 1 ? "" : "es"} for ${state.duplicateFolderPath}`));
    const disclosure = byID("duplicate-ignored-overlaps-disclosure");
    disclosure.hidden = ignored.length === 0;
    byID("duplicate-ignored-overlap-count").textContent = String(ignored.length);
    byID("duplicate-overlaps-more").hidden = !state.duplicateOverlapsCursor;
    if (!state.duplicateOverlapsLoading && state.duplicateOverlaps.length === 0) showState("duplicate-overlaps-state", "No overlapping folders were found.");
    else if (state.duplicateOverlaps.length) showState("duplicate-overlaps-state", "");
  }

  function compareDuplicateCandidates(left, right) {
    if (left.comparison.exact !== right.comparison.exact) return left.comparison.exact ? -1 : 1;
    const leftUnion = left.comparison.commonFiles + left.comparison.leftOnlyFiles + left.comparison.rightOnlyFiles;
    const rightUnion = right.comparison.commonFiles + right.comparison.leftOnlyFiles + right.comparison.rightOnlyFiles;
    const leftRatio = leftUnion ? left.comparison.commonFiles / leftUnion : 0;
    const rightRatio = rightUnion ? right.comparison.commonFiles / rightUnion : 0;
    if (leftRatio !== rightRatio) return rightRatio - leftRatio;
    if (left.comparison.commonBytes !== right.comparison.commonBytes) return right.comparison.commonBytes - left.comparison.commonBytes;
    return left.comparison.right.path.localeCompare(right.comparison.right.path);
  }

  function renderDuplicateOverlapCollection(targetID, candidates, ignored) {
    const target = byID(targetID);
    const fragment = document.createDocumentFragment();
    for (const candidate of candidates) fragment.append(duplicateOverlapCard(candidate, ignored));
    target.replaceChildren(fragment);
  }

  function duplicateComparisonDisplay(comparison) {
    const selectedIsLeft = comparison.left.path === state.duplicateFolderPath && comparison.left.area === "live";
    return {
      selected: selectedIsLeft ? comparison.left : comparison.right,
      other: selectedIsLeft ? comparison.right : comparison.left,
      selectedOnlyFiles: selectedIsLeft ? comparison.leftOnlyFiles : comparison.rightOnlyFiles,
      selectedOnlyBytes: selectedIsLeft ? comparison.leftOnlyBytes : comparison.rightOnlyBytes,
      otherOnlyFiles: selectedIsLeft ? comparison.rightOnlyFiles : comparison.leftOnlyFiles,
      otherOnlyBytes: selectedIsLeft ? comparison.rightOnlyBytes : comparison.leftOnlyBytes,
    };
  }

  function duplicateOverlapCard(candidate, ignored) {
    const comparison = candidate.comparison;
    const display = duplicateComparisonDisplay(comparison);
    const unionFiles = comparison.commonFiles + comparison.leftOnlyFiles + comparison.rightOnlyFiles;
    const percent = unionFiles ? Math.round((comparison.commonFiles / unionFiles) * 100) : 0;
    const card = document.createElement("article");
    card.className = "duplicate-card";
    const header = document.createElement("div");
    header.className = "duplicate-card-header";
    const description = document.createElement("div");
    const title = document.createElement("div");
    title.className = "duplicate-card-title";
    let relationship = "Partial overlap";
    if (comparison.exact) relationship = "Exact folder copy";
    else if (display.selectedOnlyFiles === 0) relationship = "Selected folder is contained here";
    else if (display.otherOnlyFiles === 0) relationship = "This folder is contained in the selection";
    title.append(text("h3", duplicatePathName(display.other.path)), text("span", comparison.exact ? "Exact copy · 100%" : `${percent}% shared`, comparison.exact ? "duplicate-badge exact" : "duplicate-badge"));
    const meta = document.createElement("div");
    meta.className = "duplicate-card-meta";
    meta.append(
      text("span", relationship),
      text("span", `${comparison.commonFiles} common ${duplicateKindLabel("file", comparison.commonFiles)} · ${formatBytes(comparison.commonBytes)}`),
      text("span", `${display.selectedOnlyFiles} unique here · ${display.otherOnlyFiles} unique there`),
    );
    description.append(title, meta);
    const actions = document.createElement("div");
    actions.className = "duplicate-card-actions";
    if (comparison.commonFiles > 0 && comparison.left.area === "live" && comparison.right.area === "live") {
      actions.append(
        button("Clean selected", () => previewDuplicateReconciliation(candidate, display.selected.path === comparison.left.path ? "left" : "right"), "quiet"),
        button("Clean match", () => previewDuplicateReconciliation(candidate, display.other.path === comparison.left.path ? "left" : "right"), "quiet"),
      );
    }
    if (!ignored) {
      actions.append(button("Ignore", () => setDuplicateOverlapIgnored(candidate, true, comparison.exact ? "group" : "pair"), "quiet"));
    } else {
      if (candidate.exactGroupIgnored) actions.append(button("Unignore exact copies", () => setDuplicateOverlapIgnored(candidate, false, "group"), "quiet"));
      if (candidate.ignored) actions.append(button("Unignore folder match", () => setDuplicateOverlapIgnored(candidate, false, "pair"), "quiet"));
    }
    header.append(description, actions);
    const body = document.createElement("div");
    body.className = "duplicate-card-body";
    const locations = document.createElement("div");
    locations.className = "duplicate-comparison";
    const selectedLocation = document.createElement("div");
    selectedLocation.className = "duplicate-location";
    selectedLocation.append(text("strong", display.selected.path), text("span", `${comparison.commonFiles} common · ${display.selectedOnlyFiles} unique · ${formatBytes(display.selectedOnlyBytes)} unique storage`));
    const otherLocation = document.createElement("div");
    otherLocation.className = "duplicate-location";
    otherLocation.append(text("strong", display.other.path), text("span", `${comparison.commonFiles} common · ${display.otherOnlyFiles} unique · ${formatBytes(display.otherOnlyBytes)} unique storage`));
    locations.append(selectedLocation, text("span", "↔", "duplicate-comparison-mark"), otherLocation);
    const meter = document.createElement("progress");
    meter.className = "duplicate-overlap-meter";
    meter.setAttribute("aria-label", `${percent}% of the union is shared by file count`);
    meter.max = 100;
    meter.value = percent;
    body.append(locations, meter);
    card.append(header, body);
    return card;
  }

  async function setDuplicateOverlapIgnored(candidate, ignored, policy) {
    const comparison = candidate.comparison;
    try {
      if (policy === "group") {
        await api(`/api/v1/duplicates/groups/${encodeURIComponent(comparison.left.groupID)}/ignore`, {
          method: "PUT",
          body: { ignored, expectedRevision: candidate.exactGroupIgnoreRevision || 0 },
        });
      } else {
        await api("/api/v1/duplicates/directories/ignore", {
          method: "PUT",
          body: {
            left: duplicateLocation(comparison.left),
            right: duplicateLocation(comparison.right),
            ignored,
            expectedRevision: candidate.ignoreRevision || 0,
          },
        });
      }
      announce(ignored ? "Folder match ignored." : "Folder match returned to active results.");
      await Promise.all([loadDuplicateGroups(false), loadDuplicateOverlaps(false)]);
    } catch (error) {
      announce(friendlyError(error, "The folder match ignore setting could not be saved."), true);
    }
  }

  async function previewDuplicateReconciliation(candidate, removeFrom) {
    const comparison = candidate.comparison;
    try {
      const preview = await api("/api/v1/duplicates/directories/reconciliation-preview", {
        method: "POST",
        body: {
          left: duplicateLocation(comparison.left),
          right: duplicateLocation(comparison.right),
          removeFrom,
          limit: 100,
          cursor: "",
        },
      });
      state.duplicateReconciliation = preview;
      renderDuplicateReconciliation();
      state.duplicateDialogOpener = document.activeElement;
      showTopLayerDialog(byID("duplicate-reconciliation-dialog"));
      byID("duplicate-reconciliation-close").focus();
    } catch (error) {
      announce(friendlyError(error, "The duplicate cleanup preview could not be prepared."), true);
    }
  }

  function renderDuplicateReconciliation() {
    const preview = state.duplicateReconciliation;
    const items = preview ? (preview.items || []) : [];
    const summary = byID("duplicate-reconciliation-summary");
    summary.replaceChildren();
    if (!preview) return;
    const removeOccurrence = preview.removeFrom === "left" ? preview.comparison.left : preview.comparison.right;
    const keepOccurrence = preview.removeFrom === "left" ? preview.comparison.right : preview.comparison.left;
    for (const [value, label] of [
      [formatBytes(preview.reclaimableBytes), "Reclaimable"],
      [String(items.length), `Common ${duplicateKindLabel("file", items.length)}`],
      [duplicatePathName(keepOccurrence.path), "Copy kept in Files"],
    ]) {
      const metric = document.createElement("div");
      metric.append(text("strong", value), text("span", label));
      summary.append(metric);
    }
    const target = byID("duplicate-reconciliation-items");
    const fragment = document.createDocumentFragment();
    for (const item of items) {
      const row = document.createElement("div");
      row.className = "duplicate-reconciliation-item";
      const paths = document.createElement("div");
      paths.append(text("strong", item.remove.path), text("span", `Keep ${item.keep.path}`));
      row.append(paths, text("span", formatBytes(item.remove.size)));
      fragment.append(row);
    }
    target.replaceChildren(fragment);
    byID("duplicate-reconciliation-title").textContent = `Review cleanup from ${removeOccurrence.path}`;
    byID("duplicate-reconciliation-more").hidden = !preview.nextCursor;
    const apply = byID("duplicate-reconciliation-apply");
    apply.textContent = `Move ${items.length} ${duplicateKindLabel("file", items.length)} to Trash`;
    apply.disabled = items.length === 0 || !preview.planToken;
  }

  function closeDuplicateReconciliation(restoreFocus = true) {
    const dialog = byID("duplicate-reconciliation-dialog");
    if (dialog.open) closeTopLayerDialog(dialog);
    state.duplicateReconciliation = null;
    byID("duplicate-reconciliation-items").replaceChildren();
    if (restoreFocus && state.duplicateDialogOpener && state.duplicateDialogOpener.focus) state.duplicateDialogOpener.focus();
  }

  async function applyDuplicateReconciliation() {
    const preview = state.duplicateReconciliation;
    if (!preview || !preview.planToken) return;
    const apply = byID("duplicate-reconciliation-apply");
    apply.disabled = true;
    try {
      const result = await api("/api/v1/duplicates/directories/reconcile", {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey() },
        body: { planToken: preview.planToken },
      });
      const items = result.items || [];
      const failed = items.filter((item) => item.state !== "succeeded").length;
      closeDuplicateReconciliation(false);
      if (failed) showToast(`${items.length - failed} duplicate files moved to Trash; ${failed} changed before cleanup.`, "warning", 12000);
      else showTrashUndo(items);
      await Promise.all([loadDuplicateGroups(false), loadDuplicateOverlaps(false)]);
      byID("duplicate-overlap-title").focus();
    } catch (error) {
      apply.disabled = false;
      announce(friendlyError(error, "The folder contents changed. Close this review and prepare a fresh cleanup plan."), true);
    }
  }
