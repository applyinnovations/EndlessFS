  function ask(options) {
    const dialog = byID("action-dialog"); const fields = byID("dialog-fields"); fields.replaceChildren();
    byID("dialog-title").textContent = options.title; byID("dialog-description").textContent = options.description || "";
    const confirmLabel = options.confirm || "Continue";
    const confirmControl = byID("dialog-confirm");
    if (options.danger) {
      confirmControl.className = "danger";
      confirmControl.replaceChildren(document.createTextNode(confirmLabel));
      confirmControl.setAttribute("aria-label", confirmLabel);
      delete confirmControl.dataset.icon;
      delete confirmControl.dataset.tooltip;
      confirmControl.removeAttribute("title");
    } else {
      confirmControl.className = "icon-button";
      setIconControl(confirmControl, options.confirmIcon || "check", confirmLabel);
    }
    for (const field of options.fields || []) {
      const label = text("label", field.label); label.htmlFor = field.id;
      const input = document.createElement("input"); input.id = field.id; input.name = field.id; input.type = field.type || "text"; input.value = field.value || ""; input.required = Boolean(field.required); if (field.maxLength) input.maxLength = field.maxLength;
      fields.append(label, input);
    }
    if (options.choiceField) fields.append(selectField(options.choiceField));
    if (options.conflictField) fields.append(selectField({ id: "conflict", label: "If a name is already in use", options: [["fail", "Stop and leave everything unchanged"], ["rename", "Choose a non-conflicting name"], ["replace", "Replace the existing item"]] }));
    if (options.output) { const output = text("output", options.output); output.id = "dialog-output"; fields.append(output); if (options.copyOutput) fields.append(iconButton("copy", "Copy link", () => copyText(options.output))); }
    state.dialogOpener = document.activeElement; showTopLayerDialog(dialog);
    const first = fields.querySelector("input, select, button"); if (first) first.focus(); else byID("dialog-confirm").focus();
    return new Promise((resolve) => {
      const cleanup = () => {
        byID("dialog-cancel").removeEventListener("click", cancel);
        byID("dialog-form").removeEventListener("submit", submit);
        dialog.removeEventListener("cancel", onCancel);
      };
      const finish = (value) => { cleanup(); closeTopLayerDialog(dialog); fields.replaceChildren(); if (state.dialogOpener && state.dialogOpener.focus) state.dialogOpener.focus(); resolve(value); };
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

  function setMobileNavigationOpen(open, restoreFocus = false) {
    const menu = byID("mobile-navigation");
    const toggle = byID("mobile-navigation-toggle");
    const shouldOpen = Boolean(open && state.user && matchMedia("(max-width: 760px)").matches);
    const wasOpen = !menu.hidden;
    menu.hidden = !shouldOpen;
    toggle.setAttribute("aria-expanded", String(shouldOpen));
    setIconControl(toggle, shouldOpen ? "x" : "menu-2", shouldOpen ? "Close navigation" : "Open navigation");
    byID("workspace").inert = shouldOpen;
    hideActionTooltip();
    if (shouldOpen && !wasOpen) {
      const current = menu.querySelector('[aria-current="page"]') || menu.querySelector("a, button");
      if (current) current.focus();
    } else if (!shouldOpen && wasOpen && restoreFocus) {
      toggle.focus();
    }
  }

  function wireEvents() {
    setupAutomaticPaging();
    byID("signin-button").addEventListener("click", signIn);
    byID("logout-button").addEventListener("click", logout); byID("settings-logout").addEventListener("click", logout);
    byID("mobile-logout-button").addEventListener("click", () => { setMobileNavigationOpen(false); logout(); });
    byID("mobile-navigation-toggle").addEventListener("click", () => setMobileNavigationOpen(byID("mobile-navigation").hidden, true));
    byID("mobile-report-issue").addEventListener("click", () => setMobileNavigationOpen(false));
    document.querySelectorAll("[data-route]").forEach((link) => link.addEventListener("click", (event) => { if (!state.user) return; event.preventDefault(); if (link.closest(".mobile-navigation")) setMobileNavigationOpen(false); setRoute(link.dataset.route); }));
    byID("mobile-navigation").addEventListener("keydown", (event) => { if (event.key === "Escape") { event.preventDefault(); setMobileNavigationOpen(false, true); } });
    window.addEventListener("resize", () => { if (!matchMedia("(max-width: 760px)").matches) setMobileNavigationOpen(false); });
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
    byID("upload-input").addEventListener("change", async (event) => {
      if (state.transferReconnectGroup !== null) await reconnectTransferSources(event.target.files);
      else await queueFiles(event.target.files);
      event.target.value = "";
    });
    byID("folder-input").addEventListener("change", async (event) => {
      if (state.transferReconnectGroup !== null) await reconnectTransferSources(event.target.files);
      else await queueFolderFiles(event.target.files);
      event.target.value = "";
    });
    const drop = byID("drop-target");
    drop.addEventListener("dragover", (event) => { if (state.browserAccess !== "owner") return; event.preventDefault(); drop.classList.add("dragging"); });
    drop.addEventListener("dragleave", () => drop.classList.remove("dragging"));
    drop.addEventListener("drop", async (event) => {
      if (state.browserAccess !== "owner") return;
      event.preventDefault();
      drop.classList.remove("dragging");
      try { await queueDroppedItems(event.dataTransfer); }
      catch (error) { announce(friendlyError(error, "Dropped files could not be read."), true); }
    });
    drop.addEventListener("keydown", (event) => { if (state.browserAccess === "owner" && event.target === drop && (event.key === "Enter" || event.key === " ")) { event.preventDefault(); byID("upload-input").click(); } });
    bindTabs(byID("transfer-filter"), (value) => {
      state.transferFilter = value;
      state.transferVirtualStart = 0;
      renderTransfers();
    });
    const transferConnection = navigator.connection || navigator.mozConnection || navigator.webkitConnection;
    if (transferConnection && transferConnection.addEventListener) transferConnection.addEventListener("change", pumpTransfers);
    byID("clear-transfers").addEventListener("click", async () => {
      const removedGroups = [];
      const removedItems = [];
      for (const [groupID, group] of state.transferGroups) {
        if (["complete", "cancelled"].includes(group.state)) {
          removedGroups.push(groupID);
          removedItems.push(...group.transferIDs);
          for (const id of group.transferIDs) state.transferByID.delete(id);
          state.transferGroups.delete(groupID);
          state.transfers = state.transfers.filter((item) => item.groupID !== groupID);
        }
      }
      state.transfers = state.transfers.filter((item) => {
        const remove = !item.groupID && ["complete", "cancelled"].includes(item.state);
        if (remove) {
          removedItems.push(item.id);
          state.transferByID.delete(item.id);
        }
        return !remove;
      });
      await deleteTransferLedgerEntries(removedItems, removedGroups);
      renderTransfers();
      setTransferSheetOpen(!byID("transfer-panel").hidden);
    });
    byID("retry-failed-transfers").addEventListener("click", retryFailedTransfers);
    byID("transfer-search").addEventListener("input", (event) => {
      state.transferSearch = event.target.value;
      state.transferVirtualStart = 0;
      renderTransfers();
    });
    byID("transfer-list").addEventListener("scroll", () => {
      const list = byID("transfer-list");
      if (list.scrollTop <= transferVirtualRowHeight && state.transferVirtualStart > 0) scheduleTransferWindow(-1);
      else if (list.scrollTop + list.clientHeight >= list.scrollHeight - transferVirtualRowHeight && state.transferVirtualStart + transferVirtualWindowSize < state.transferProjection.length) scheduleTransferWindow(1);
    });
    byID("open-transfers").addEventListener("click", () => { setMobileNavigationOpen(false); setTransferSheetOpen(byID("transfer-panel").hidden, true); });
    byID("transfer-close").addEventListener("click", () => setTransferSheetOpen(false, true));
    byID("transfer-panel").addEventListener("keydown", (event) => {
      if (event.key === "Escape") {
        event.preventDefault();
        setTransferSheetOpen(false, true);
        return;
      }
      if (event.key !== "Tab" || !matchMedia("(max-width: 760px)").matches) return;
      const controls = Array.from(byID("transfer-panel").querySelectorAll("button:not(:disabled), input:not(:disabled), [role=tab][tabindex='0']"));
      if (!controls.length) return;
      const first = controls[0];
      const last = controls[controls.length - 1];
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
    });
    byID("file-filter").addEventListener("input", renderFiles); byID("file-sort").addEventListener("change", () => state.browserAccess === "owner" ? loadDirectory(state.currentDirectory) : renderFiles());
    for (const id of ["filter-kind", "filter-media", "filter-min-size", "filter-max-size", "filter-modified-after", "filter-modified-before", "filter-preview"]) byID(id).addEventListener("input", renderFiles);
    for (const id of ["file-view-list", "file-view-grid"]) byID(id).addEventListener("change", (event) => { if (event.target.checked) { state.viewMode = event.target.value; renderFiles(); } });
    byID("media-grid").addEventListener("scroll", () => { if (!state.gridRenderFrame) state.gridRenderFrame = requestAnimationFrame(() => { state.gridRenderFrame = 0; if (state.viewMode === "grid") renderVirtualGrid(state.filteredEntries); }); maybeLoadNextBrowserPage(byID("media-grid")); });
    byID("list-presentation").addEventListener("scroll", () => {
      if (!state.listRenderFrame) state.listRenderFrame = requestAnimationFrame(() => { state.listRenderFrame = 0; if (state.viewMode === "list") renderVirtualList(state.filteredEntries); });
      maybeLoadNextBrowserPage(byID("list-presentation"));
    });
    window.addEventListener("resize", () => {
      if (state.viewMode === "grid") renderVirtualGrid(state.filteredEntries);
      if (!byID("settings-view").hidden) { renderPasskeys(); renderShares(); }
      if (!byID("admin-view").hidden) renderUsers();
      if (!byID("transfer-panel").hidden) setTransferSheetOpen(true);
    });
    byID("select-all").addEventListener("change", (event) => { for (const entry of filterLoadedEntries(state.entries)) { const key = entrySelectionKey(entry); if (event.target.checked) state.selected.set(key, entry); else state.selected.delete(key); } renderFiles(); updateSelection(); });
    byID("clear-selection").addEventListener("click", () => { state.selected.clear(); renderFiles(); updateSelection(); });
    byID("download-selected").addEventListener("click", () => download([...state.selected.values()][0]));
    byID("share-selected").addEventListener("click", () => createShare([...state.selected.values()][0]));
    byID("copy-selected").addEventListener("click", () => copyMove(false)); byID("move-selected").addEventListener("click", () => copyMove(true)); byID("trash-selected").addEventListener("click", trashSelected);
    byID("restore-selected").addEventListener("click", restoreSelectedTrash);
    byID("delete-selected-permanently").addEventListener("click", deleteSelectedTrash);
    byID("empty-trash").addEventListener("click", emptyTrash);
    byID("profile-form").addEventListener("submit", async (event) => { event.preventDefault(); try { const profile = await api("/api/v1/me", { method: "PATCH", body: { displayName: byID("profile-name").value } }); state.user.displayName = profile.displayName; announce("Display name saved."); } catch (error) { announce(friendlyError(error), true); } });
    byID("theme-form").addEventListener("submit", async (event) => { event.preventDefault(); try { const dark = matchMedia("(prefers-color-scheme: dark)").matches; state.safeTheme = byID("safe-theme").checked; syncSafeThemeURL(); const selection = state.safeTheme ? await api(themePreferenceURL(dark)) : await api("/api/v1/me/preferences/theme", { method: "PUT", body: { themeID: byID("theme-select").value, dark } }); applyTheme(selection); byID("theme-note").textContent = `Using ${selection.resolved.name}.`; announce("Theme applied."); } catch (error) { announce(friendlyError(error), true); } });
    byID("safe-theme").addEventListener("change", () => byID("theme-form").requestSubmit());
    byID("theme-select").addEventListener("change", () => { byID("safe-theme").checked = false; });
    byID("refresh-shares").addEventListener("click", loadSettings);
    byID("passkey-table-scroll").addEventListener("scroll", () => {
      if (state.passkeyRenderFrame) return;
      state.passkeyRenderFrame = requestAnimationFrame(() => { state.passkeyRenderFrame = 0; renderPasskeys(); });
    });
    byID("share-table-scroll").addEventListener("scroll", () => {
      if (state.shareRenderFrame) return;
      state.shareRenderFrame = requestAnimationFrame(() => { state.shareRenderFrame = 0; renderShares(); });
    });
    byID("invite-table-scroll").addEventListener("scroll", () => {
      if (state.inviteRenderFrame) return;
      state.inviteRenderFrame = requestAnimationFrame(() => { state.inviteRenderFrame = 0; renderInvites(); });
    });
    byID("add-passkey").addEventListener("click", async () => { const result = await ask({ title: "Add a passkey", description: "Give this passkey an optional device label.", confirm: "Create passkey", confirmIcon: "key-plus", fields: [{ id: "passkey-label", label: "Label (optional)", maxLength: 100 }] }); if (!result) return; try { await register("add", "", "", result["passkey-label"]); announce("Passkey added and session renewed."); await loadSettings(); } catch (error) { announce(friendlyError(error), true); } });
    byID("create-invite").addEventListener("click", async () => { const result = await ask({ title: "Create an invite", description: "The link can create one account. Optionally set an expiry.", confirm: "Create invite", confirmIcon: "user-plus", fields: [{ id: "invite-expires", label: "Expiry (optional)", type: "datetime-local" }] }); if (!result) return; try { const body = {}; if (result["invite-expires"]) body.expiresAt = new Date(result["invite-expires"]).toISOString(); const created = await api("/api/v1/admin/invites", { method: "POST", headers: { "Idempotency-Key": idempotencyKey() }, body }); await revealLink("Invite link", created.link); announce("Invite created."); await loadAdmin(); } catch (error) { announce(friendlyError(error), true); } });
    byID("copy-invite").addEventListener("click", () => copyText(byID("invite-link").textContent));
    byID("refresh-users").addEventListener("click", loadAdmin);
    byID("users-table-scroll").addEventListener("scroll", () => {
      if (!state.userRenderFrame) state.userRenderFrame = requestAnimationFrame(() => { state.userRenderFrame = 0; renderUsers(); });
      const scroller = byID("users-table-scroll");
      if (scroller.scrollTop + scroller.clientHeight >= scroller.scrollHeight - (settingsRowHeight * 6)) loadMoreUsers();
    });
    const closePreview = () => { if (state.viewerController) state.viewerController.abort(); state.viewerController = null; releaseViewerObjectURL(); byID("preview-content").replaceChildren(); closeTopLayerDialog(byID("preview-dialog")); if (state.viewerOpener && state.viewerOpener.focus) state.viewerOpener.focus(); state.viewerEntry = null; };
    byID("preview-previous").addEventListener("click", () => navigateViewer(-1));
    byID("preview-next").addEventListener("click", () => navigateViewer(1));
    byID("preview-generate").addEventListener("click", () => generateViewerPreview(false));
    byID("preview-regenerate").addEventListener("click", () => generateViewerPreview(true));
    byID("preview-original").addEventListener("click", () => { if (state.viewerEntry) openSafeOriginalPreview(state.viewerEntry); });
    byID("preview-close").addEventListener("click", closePreview);
    byID("preview-dialog").addEventListener("cancel", (event) => { event.preventDefault(); closePreview(); });
    byID("preview-dialog").addEventListener("keydown", (event) => { if (event.key === "ArrowLeft") { event.preventDefault(); navigateViewer(-1); } if (event.key === "ArrowRight") { event.preventDefault(); navigateViewer(1); } });
    window.addEventListener("popstate", () => { if (state.user) setRoute(routeFromPath(), false); });
    window.addEventListener("online", resumePausedTransfers);
    window.addEventListener("offline", () => {
      for (const transfer of state.transfers.filter((item) => item.state === "retry-wait")) transitionTransfer(transfer, "paused", "Waiting for a network connection.", "offline");
      renderTransfers();
    });
  }

  async function revokeInvite(invite) {
    const confirmed = await ask({ title: "Revoke this invite?", description: "The invite link will stop working immediately.", confirm: "Revoke Invite", danger: true }); if (!confirmed) return;
    try { await api(`/api/v1/admin/invites/${encodeURIComponent(invite.inviteID)}`, { method: "DELETE" }); announce("Invite revoked."); await loadAdmin(); } catch (error) { announce(friendlyError(error), true); }
  }

  function configureRegistration() {
    const bootstrap = location.pathname === "/bootstrap";
    const recovery = location.pathname === "/recover" || Boolean(state.recoveryToken);
    byID("bootstrap-token-field").hidden = !bootstrap;
    byID("bootstrap-token").required = bootstrap;
    byID("display-name-field").hidden = recovery;
    byID("display-name").required = !recovery;
    byID("registration-signin").hidden = bootstrap;
    byID("registration-title").textContent = bootstrap ? "Initialize EndlessFS" : recovery ? "Replace passkey" : state.inviteToken ? "Accept invite" : "Create account";
    byID("registration-submit").textContent = bootstrap ? "Create administrator" : recovery ? "Create replacement passkey" : "Create passkey";
    byID("registration-help").textContent = recovery ? "The recovery link identifies your account. No display name is requested." : "Enter a display name.";
  }

  async function enterApplication() {
    byID("auth-view").hidden = true; byID("registration-view").hidden = true; byID("public-view").hidden = true;
    byID("authenticated-view").hidden = false; byID("loading-view").hidden = false;
    byID("account-actions").hidden = false;
    const admin = (state.user.roles || []).includes("admin"); byID("admin-nav").hidden = !admin; byID("mobile-admin-nav").hidden = !admin;
    const routeLoad = setRoute(routeFromPath(), false);
    const dark = matchMedia("(prefers-color-scheme: dark)").matches;
    const themeLoad = api(themePreferenceURL(dark)).then(applyTheme).catch(() => announce("Your selected theme could not be loaded; a built-in appearance remains active."));
    await Promise.all([routeLoad, themeLoad]);
    await restoreTransferLedger();
    seedTransferPreviewFixture();
    if (document.fonts && document.fonts.ready) await document.fonts.ready;
    byID("loading-view").hidden = true;
    byID("app").dataset.state = "authenticated";
  }

  async function start() {
    consumePathTokens(); wireIconControls(); wireActionTooltips(); wireEvents();
    try { state.config = await api("/api/v1/config"); }
    catch (error) { showState("drive-state", friendlyError(error, "EndlessFS configuration is unavailable."), "error"); }
    if (state.publicToken) { loadPublicShare(); return; }
    if (["/bootstrap", "/register", "/recover"].includes(location.pathname) || state.inviteToken || state.recoveryToken) { configureRegistration(); showOnly("registration-view"); byID("display-name").focus(); return; }
    try { state.user = await api("/api/v1/me"); await enterApplication(); }
    catch (error) {
      if (!(error instanceof APIError) || error.status !== 401) announce(friendlyError(error), true);
      showOnly("auth-view"); byID("account-actions").hidden = true; byID("register-link").hidden = state.config ? !state.config.allowRegistration : false; byID("signin-button").focus();
    }
  }

  window.addEventListener("DOMContentLoaded", start);
})();
