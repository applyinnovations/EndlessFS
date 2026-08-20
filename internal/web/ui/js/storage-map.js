  const storageMapMaximumTiles = 180;
  const storageMapMinimumTileArea = 420;
  const storageMapExpandedMinimumWidth = 180;
  const storageMapExpandedMinimumHeight = 120;
  const storageMapExpandedHeaderHeight = 26;
  const storageMapNamespace = "http://www.w3.org/2000/svg";

  function storageMapElement(name, className = "") {
    const node = document.createElementNS(storageMapNamespace, name);
    if (className) node.setAttribute("class", className);
    return node;
  }

  function storageMapFiltersActive() {
    return Boolean(
      byID("file-filter").value.trim() ||
      byID("filter-kind").value ||
      byID("filter-media").value ||
      byID("filter-min-size").value ||
      byID("filter-max-size").value ||
      byID("filter-modified-after").value ||
      byID("filter-modified-before").value ||
      byID("filter-preview").value
    );
  }

  function storageMapAggregate(entries) {
    let size = 0;
    let fileCount = 0;
    for (const entry of entries) {
      const entrySize = knownEntrySize(entry);
      const entryFileCount = knownEntryFileCount(entry);
      if (entrySize === null || entryFileCount === null || !Number.isSafeInteger(size + entrySize) || !Number.isSafeInteger(fileCount + entryFileCount)) return null;
      size += entrySize;
      fileCount += entryFileCount;
    }
    return { size, fileCount };
  }

  function storageMapRemainder(entries, renderedEntries, current) {
    const currentSize = knownEntrySize(current);
    const currentFileCount = knownEntryFileCount(current);
    if (currentSize === null || currentFileCount === null) return null;
    let renderedSize = 0;
    let renderedFileCount = 0;
    for (const entry of renderedEntries) {
      const size = knownEntrySize(entry);
      const fileCount = knownEntryFileCount(entry);
      if (size === null || fileCount === null || !entries.includes(entry)) return null;
      renderedSize += size;
      renderedFileCount += fileCount;
    }
    if (!Number.isSafeInteger(renderedSize) || !Number.isSafeInteger(renderedFileCount) || renderedSize > currentSize || renderedFileCount > currentFileCount) return null;
    return { size: currentSize - renderedSize, fileCount: currentFileCount - renderedFileCount };
  }

  function storageMapWorstAspect(row, shortSide) {
    if (!row.length || shortSide <= 0) return Number.POSITIVE_INFINITY;
    let sum = 0;
    let minimum = Number.POSITIVE_INFINITY;
    let maximum = 0;
    for (const item of row) {
      sum += item.area;
      minimum = Math.min(minimum, item.area);
      maximum = Math.max(maximum, item.area);
    }
    if (sum <= 0 || minimum <= 0) return Number.POSITIVE_INFINITY;
    const squaredSum = sum * sum;
    const squaredSide = shortSide * shortSide;
    return Math.max((squaredSide * maximum) / squaredSum, squaredSum / (squaredSide * minimum));
  }

  function storageMapPlaceRow(row, remaining, layouts) {
    const area = row.reduce((sum, item) => sum + item.area, 0);
    if (remaining.width >= remaining.height) {
      const rowWidth = remaining.height > 0 ? area / remaining.height : 0;
      let y = remaining.y;
      for (const item of row) {
        const height = rowWidth > 0 ? item.area / rowWidth : 0;
        layouts.push({ item: item.item, x: remaining.x, y, width: rowWidth, height });
        y += height;
      }
      return { x: remaining.x + rowWidth, y: remaining.y, width: Math.max(0, remaining.width - rowWidth), height: remaining.height };
    }
    const rowHeight = remaining.width > 0 ? area / remaining.width : 0;
    let x = remaining.x;
    for (const item of row) {
      const width = rowHeight > 0 ? item.area / rowHeight : 0;
      layouts.push({ item: item.item, x, y: remaining.y, width, height: rowHeight });
      x += width;
    }
    return { x: remaining.x, y: remaining.y + rowHeight, width: remaining.width, height: Math.max(0, remaining.height - rowHeight) };
  }

  function layoutStorageMap(items, width, height) {
    const total = items.reduce((sum, item) => sum + item.size, 0);
    if (!items.length || total <= 0 || width <= 0 || height <= 0) return [];
    const scale = width * height / total;
    const pending = items.map((item) => ({ item, area: item.size * scale }));
    const layouts = [];
    let remaining = { x: 0, y: 0, width, height };
    let row = [];
    while (pending.length) {
      const next = pending[0];
      const shortSide = Math.min(remaining.width, remaining.height);
      if (!row.length || storageMapWorstAspect(row.concat(next), shortSide) <= storageMapWorstAspect(row, shortSide)) {
        row.push(pending.shift());
      } else {
        remaining = storageMapPlaceRow(row, remaining, layouts);
        row = [];
      }
    }
    if (row.length) storageMapPlaceRow(row, remaining, layouts);
    return layouts;
  }

  function storageMapDimensions() {
    const map = byID("storage-map");
    return {
      width: Math.max(320, Math.round(map.clientWidth || 960)),
      height: Math.max(280, Math.round(map.clientHeight || 560)),
    };
  }

  function storageMapCandidateEntries(entries, totalSize, area, maximumTiles = storageMapMaximumTiles) {
    const candidates = entries
      .filter((entry) => knownEntrySize(entry) !== null && knownEntrySize(entry) > 0)
      .slice()
      .sort((left, right) => compareEntrySizes(left, right, -1) || left.path.localeCompare(right.path));
    const rendered = [];
    for (const entry of candidates) {
      if (rendered.length >= maximumTiles) break;
      if (knownEntrySize(entry) / totalSize * area < storageMapMinimumTileArea) break;
      rendered.push(entry);
    }
    return rendered;
  }

  function storageMapMaximumOmittedSize(entries, renderedEntries, serverMaximumSize) {
    const rendered = new Set(renderedEntries);
    let maximum = Number.isSafeInteger(serverMaximumSize) && serverMaximumSize >= 0 ? serverMaximumSize : null;
    for (const entry of entries) {
      if (rendered.has(entry)) continue;
      const size = knownEntrySize(entry);
      if (size !== null && (maximum === null || size > maximum)) maximum = size;
    }
    return maximum;
  }

  function openStorageMapRemainder(item) {
    if (!Number.isSafeInteger(item.maximumSize) || item.maximumSize < 0 || !item.directory) return;
    byID("file-filter").value = "";
    for (const id of ["filter-kind", "filter-media", "filter-min-size", "filter-modified-after", "filter-modified-before", "filter-preview"]) byID(id).value = "";
    byID("filter-max-size").value = String(item.maximumSize);
    const disclosure = byID("metadata-filters").closest("details");
    if (disclosure) disclosure.open = false;
    setFileViewMode("list", false, false);
    byID("file-sort").value = "size:desc";
    syncBrowserURLState("push", "owner");
    loadDirectory(item.directory, false, true);
  }

  function storageMapChildItems(entry, layout, remainingNodeBudget) {
    const children = Array.isArray(entry.children) ? entry.children : [];
    if (
      children.length === 0 ||
      remainingNodeBudget < 2 ||
      layout.width < storageMapExpandedMinimumWidth ||
      layout.height < storageMapExpandedMinimumHeight
    ) return null;
    const content = {
      x: layout.x + 3,
      y: layout.y + storageMapExpandedHeaderHeight,
      width: Math.max(0, layout.width - 6),
      height: Math.max(0, layout.height - storageMapExpandedHeaderHeight - 3),
    };
    const totalSize = knownEntrySize(entry);
    if (totalSize === null || totalSize === 0 || content.width * content.height < storageMapMinimumTileArea * 2) return null;
    const renderedEntries = storageMapCandidateEntries(children, totalSize, content.width * content.height, remainingNodeBudget - 1);
    const remainder = storageMapRemainder(children, renderedEntries, entry);
    if (!remainder) return null;
    const items = renderedEntries.map((child) => ({ entry: child, size: knownEntrySize(child), fileCount: knownEntryFileCount(child) }));
    const maximumSize = storageMapMaximumOmittedSize(children, renderedEntries, entry.remainingMaximumSize);
    if (remainder.size > 0 && maximumSize !== null && items.length < remainingNodeBudget) items.push({ aggregate: true, size: remainder.size, fileCount: remainder.fileCount, directory: entry.path, maximumSize });
    if (items.length < 2) return null;
    const layouts = layoutStorageMap(items, content.width, content.height).map((childLayout) => ({
      ...childLayout,
      x: childLayout.x + content.x,
      y: childLayout.y + content.y,
    }));
    return { layouts, renderedEntries };
  }

  function storageMapLabel(item) {
    if (item.aggregate) {
      const files = `${item.fileCount.toLocaleString()} ${item.fileCount === 1 ? "file" : "files"}`;
      const threshold = Number.isSafeInteger(item.maximumSize) ? `, show items ${formatBytes(item.maximumSize)} or smaller` : "";
      return `Remaining items, ${formatBytes(item.size)}, ${files}${threshold}`;
    }
    const kind = item.entry.kind === "directory" ? "Folder" : "File";
    const parts = [`${kind} ${item.entry.name}`, formatEntrySize(item.entry)];
    if (item.entry.kind === "directory") parts.push(formatEntryFileCount(item.entry));
    return parts.join(", ");
  }

  function storageMapText(value, x, y, className) {
    const node = storageMapElement("text", className);
    node.setAttribute("x", x.toFixed(2));
    node.setAttribute("y", y.toFixed(2));
    node.textContent = value;
    return node;
  }

  function storageMapTruncatedName(value, width) {
    const maximum = Math.max(1, Math.floor((width - 22) / 7));
    if (value.length <= maximum) return value;
    return `${value.slice(0, Math.max(1, maximum - 1))}…`;
  }

  function toggleStorageMapSelection(entry, restoreFocus = false) {
    const key = entrySelectionKey(entry);
    if (state.selected.has(key)) state.selected.delete(key);
    else state.selected.set(key, entry);
    renderFiles();
    updateSelection();
    if (restoreFocus) requestAnimationFrame(() => {
      const tile = Array.from(byID("storage-map-canvas").querySelectorAll("[data-storage-entry]"))
        .find((candidate) => candidate.dataset.storageEntry === entry.path);
      if (tile) tile.focus();
    });
  }

  function focusStorageMapNeighbor(tile, layouts, direction) {
    const index = Number.parseInt(tile.dataset.storageIndex, 10);
    const current = layouts[index];
    if (!current) return;
    const currentX = current.x + current.width / 2;
    const currentY = current.y + current.height / 2;
    let best = null;
    for (let candidateIndex = 0; candidateIndex < layouts.length; candidateIndex += 1) {
      const candidate = layouts[candidateIndex];
      if (candidateIndex === index) continue;
      const x = candidate.x + candidate.width / 2;
      const y = candidate.y + candidate.height / 2;
      const primary = direction === "ArrowLeft" ? currentX - x : direction === "ArrowRight" ? x - currentX : direction === "ArrowUp" ? currentY - y : y - currentY;
      if (primary <= 0) continue;
      const secondary = direction === "ArrowLeft" || direction === "ArrowRight" ? Math.abs(y - currentY) : Math.abs(x - currentX);
      const score = primary * 1000 + secondary;
      if (!best || score < best.score) best = { index: candidateIndex, score };
    }
    if (best) byID("storage-map-canvas").querySelector(`[data-storage-index="${best.index}"]`).focus();
  }

  function storageMapTile(layout, index, layouts, definitions, level, position, setSize, expanded = false, stopPropagation = false) {
    const item = layout.item;
    const entry = item.entry;
    const selected = Boolean(entry && state.selected.has(entrySelectionKey(entry)));
    const classNames = ["storage-map-tile", item.aggregate ? "aggregate" : entry.kind, selected ? "selected" : ""].filter(Boolean);
    if (expanded) classNames.push("expanded");
    const tile = storageMapElement("g", classNames.join(" "));
    tile.dataset.storageIndex = String(index);
    tile.dataset.tooltip = storageMapLabel(item);
    tile.setAttribute("role", "treeitem");
    tile.setAttribute("aria-level", String(level));
    tile.setAttribute("aria-posinset", String(position));
    tile.setAttribute("aria-setsize", String(setSize));
    tile.setAttribute("aria-label", storageMapLabel(item));
    tile.setAttribute("aria-selected", String(selected));
    if (expanded) tile.setAttribute("aria-expanded", "true");
    if (item.aggregate) {
      tile.setAttribute("tabindex", "0");
      tile.addEventListener("click", (event) => {
        if (stopPropagation) event.stopPropagation();
        openStorageMapRemainder(item);
      });
      tile.addEventListener("keydown", (event) => {
        if (stopPropagation) event.stopPropagation();
        if (event.key === "Enter" || event.key === " ") { event.preventDefault(); openStorageMapRemainder(item); }
        else if (["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(event.key)) { event.preventDefault(); focusStorageMapNeighbor(tile, layouts, event.key); }
      });
    } else {
      tile.dataset.storageEntry = entry.path;
      tile.setAttribute("tabindex", "0");
      tile.addEventListener("click", (event) => {
        if (stopPropagation) event.stopPropagation();
        openBrowserEntry(entry);
      });
      tile.addEventListener("keydown", (event) => {
        if (stopPropagation) event.stopPropagation();
        if (event.key === "Enter") { event.preventDefault(); openBrowserEntry(entry); }
        else if (event.key === " ") { event.preventDefault(); toggleStorageMapSelection(entry, true); }
        else if (["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(event.key)) { event.preventDefault(); focusStorageMapNeighbor(tile, layouts, event.key); }
      });
    }

    const inset = 1;
    const shape = storageMapElement("rect", expanded ? "storage-map-group-shape" : "storage-map-shape");
    shape.setAttribute("x", (layout.x + inset).toFixed(2));
    shape.setAttribute("y", (layout.y + inset).toFixed(2));
    shape.setAttribute("width", Math.max(0, layout.width - inset * 2).toFixed(2));
    shape.setAttribute("height", Math.max(0, layout.height - inset * 2).toFixed(2));
    tile.append(shape);

    const clipID = `storage-map-clip-${state.storageMapRenderVersion}-${index}`;
    const clip = storageMapElement("clipPath");
    clip.setAttribute("id", clipID);
    const clipShape = storageMapElement("rect");
    clipShape.setAttribute("x", (layout.x + 6).toFixed(2));
    clipShape.setAttribute("y", (layout.y + 4).toFixed(2));
    clipShape.setAttribute("width", Math.max(0, layout.width - 12).toFixed(2));
    clipShape.setAttribute("height", Math.max(0, layout.height - 8).toFixed(2));
    clip.append(clipShape);
    definitions.append(clip);

    if (layout.width >= 58 && layout.height >= 30) {
      const labels = storageMapElement("g", "storage-map-labels");
      labels.setAttribute("clip-path", `url(#${clipID})`);
      const name = item.aggregate ? "Remaining items" : entry.name;
      labels.append(storageMapText(storageMapTruncatedName(name, layout.width), layout.x + 9, layout.y + 20, "storage-map-name"));
      if (!expanded && layout.height >= 50) labels.append(storageMapText(formatBytes(item.size), layout.x + 9, layout.y + 39, "storage-map-size"));
      if (!expanded && layout.height >= 69 && item.fileCount > 0 && (item.aggregate || entry.kind === "directory")) labels.append(storageMapText(formatEntryFileCount(item.aggregate ? item : entry), layout.x + 9, layout.y + 57, "storage-map-count"));
      tile.append(labels);
    }

    if (entry && layout.width >= 36 && layout.height >= 36) {
      const selector = storageMapElement("foreignObject", "storage-map-select-control");
      selector.setAttribute("x", Math.max(layout.x + 3, layout.x + layout.width - 31).toFixed(2));
      selector.setAttribute("y", (layout.y + 3).toFixed(2));
      selector.setAttribute("width", "28");
      selector.setAttribute("height", "28");
      const checkbox = document.createElement("input");
      checkbox.className = "storage-map-checkbox";
      checkbox.type = "checkbox";
      checkbox.checked = selected;
      checkbox.setAttribute("aria-label", `Select ${entry.name}`);
      checkbox.addEventListener("click", (event) => event.stopPropagation());
      checkbox.addEventListener("change", () => toggleStorageMapSelection(entry, true));
      selector.append(checkbox);
      tile.append(selector);
    }
    return tile;
  }

  function renderStorageMapMessage(message) {
    const canvas = byID("storage-map-canvas");
    const dimensions = storageMapDimensions();
    canvas.setAttribute("viewBox", `0 0 ${dimensions.width} ${dimensions.height}`);
    const label = storageMapText(message, dimensions.width / 2, dimensions.height / 2, "storage-map-message");
    label.setAttribute("text-anchor", "middle");
    canvas.replaceChildren(label);
    byID("storage-map").removeAttribute("aria-busy");
  }

  function renderStorageMap(entries) {
    const map = byID("storage-map");
    if (map.hidden || state.browserAccess !== "owner") return;
    const canvas = byID("storage-map-canvas");
    const dimensions = storageMapDimensions();
    const filtered = storageMapFiltersActive();
    const current = filtered ? storageMapAggregate(entries) : state.currentEntry;
    const totalSize = knownEntrySize(current);
    if (totalSize === null) { renderStorageMapMessage("Storage map unavailable"); return; }
    if (totalSize === 0) { renderStorageMapMessage("0 B used"); return; }

    const renderedEntries = storageMapCandidateEntries(entries, totalSize, dimensions.width * dimensions.height);
    const items = renderedEntries.map((entry) => ({ entry, size: knownEntrySize(entry), fileCount: knownEntryFileCount(entry) }));
    const remainder = storageMapRemainder(entries, renderedEntries, current);
    if (!remainder) { renderStorageMapMessage("Storage map unavailable"); return; }
    const maximumSize = storageMapMaximumOmittedSize(entries, renderedEntries, state.storageMapRemainingMaximumSize);
    if (remainder.size > 0 && maximumSize !== null) items.push({ aggregate: true, size: remainder.size, fileCount: remainder.fileCount, directory: state.currentDirectory, maximumSize });
    if (!items.length) { renderStorageMapMessage("0 B used"); return; }

    state.storageMapRenderVersion += 1;
    const rootLayouts = layoutStorageMap(items, dimensions.width, dimensions.height);
    let remainingNodeBudget = Math.max(0, storageMapMaximumTiles - rootLayouts.length);
    const renderTree = rootLayouts.map((layout) => {
      const entry = layout.item.entry;
      const children = !filtered && entry && entry.kind === "directory"
        ? storageMapChildItems(entry, layout, remainingNodeBudget)
        : null;
      if (children) remainingNodeBudget -= children.layouts.length;
      return { layout, children };
    });
    const focusLayouts = [];
    for (const node of renderTree) {
      node.layout.storageIndex = focusLayouts.length;
      focusLayouts.push(node.layout);
      if (node.children) for (const childLayout of node.children.layouts) {
        childLayout.storageIndex = focusLayouts.length;
        focusLayouts.push(childLayout);
      }
    }
    const definitions = storageMapElement("defs");
    const fragment = document.createDocumentFragment();
    fragment.append(definitions);
    for (let rootIndex = 0; rootIndex < renderTree.length; rootIndex += 1) {
      const node = renderTree[rootIndex];
      const tile = storageMapTile(node.layout, node.layout.storageIndex, focusLayouts, definitions, 1, rootIndex + 1, renderTree.length, Boolean(node.children));
      if (node.children) {
        const group = storageMapElement("g", "storage-map-children");
        group.setAttribute("role", "group");
        for (let childIndex = 0; childIndex < node.children.layouts.length; childIndex += 1) {
          const childLayout = node.children.layouts[childIndex];
          group.append(storageMapTile(childLayout, childLayout.storageIndex, focusLayouts, definitions, 2, childIndex + 1, node.children.layouts.length, false, true));
        }
        tile.append(group);
      }
      fragment.append(tile);
    }
    canvas.setAttribute("viewBox", `0 0 ${dimensions.width} ${dimensions.height}`);
    canvas.replaceChildren(fragment);
    map.removeAttribute("aria-busy");
  }

  function renderStorageMapLoading() {
    const map = byID("storage-map");
    const canvas = byID("storage-map-canvas");
    const dimensions = storageMapDimensions();
    const items = [34, 22, 16, 12, 9, 7].map((size) => ({ size }));
    const layouts = layoutStorageMap(items, dimensions.width, dimensions.height);
    const fragment = document.createDocumentFragment();
    for (const layout of layouts) {
      const shape = storageMapElement("rect", "storage-map-loading-shape");
      shape.setAttribute("x", (layout.x + 1).toFixed(2));
      shape.setAttribute("y", (layout.y + 1).toFixed(2));
      shape.setAttribute("width", Math.max(0, layout.width - 2).toFixed(2));
      shape.setAttribute("height", Math.max(0, layout.height - 2).toFixed(2));
      fragment.append(shape);
    }
    canvas.setAttribute("viewBox", `0 0 ${dimensions.width} ${dimensions.height}`);
    canvas.replaceChildren(fragment);
    map.setAttribute("aria-busy", "true");
  }

  function setFileViewMode(mode, reload = true, syncURL = true) {
    const previous = state.viewMode;
    const next = mode === "storage" && state.browserAccess !== "owner" ? "list" : mode;
    const sort = byID("file-sort");
    if (next === "storage") {
      if (previous !== "storage") state.fileSortBeforeStorage = sort.value;
      sort.value = "size:desc";
      sort.disabled = true;
    } else {
      sort.disabled = false;
      if (previous === "storage" && state.fileSortBeforeStorage) sort.value = state.fileSortBeforeStorage;
    }
    state.viewMode = next;
    const control = byID(`file-view-${next}`);
    if (control) control.checked = true;
    syncFilePresentation();
    if (syncURL && previous !== next) {
      syncBrowserURLState("push");
    }
    if (!reload) return;
    if (state.browserAccess === "owner" && (previous === "storage" || next === "storage")) loadDirectory(state.currentDirectory, false, true);
    else renderFiles();
  }
