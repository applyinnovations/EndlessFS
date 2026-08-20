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
    byID("passkey-table-scroll").scrollTop = 0;
    byID("share-table-scroll").scrollTop = 0;
    renderTableLoadingRows("passkey-list", "passkey", 4, 3);
    renderTableLoadingRows("share-list", "share", 6, 3);
    try {
      const dark = matchMedia("(prefers-color-scheme: dark)").matches;
      const [catalog, preference, passkeys, shares] = await Promise.all([
        api("/api/v1/themes"), api(themePreferenceURL(dark)), api("/api/v1/me/passkeys"), api("/api/v1/shares"),
      ]);
      state.themes = catalog.themes || [];
      const select = byID("theme-select"); select.replaceChildren();
      const system = text("option", "Follow system"); system.value = "system"; select.append(system);
      for (const theme of state.themes) { const option = text("option", `${theme.name} (${theme.appearance})`); option.value = theme.id; select.append(option); }
      select.value = preference.preference;
      byID("safe-theme").checked = state.safeTheme;
      applyTheme(preference);
      byID("theme-note").textContent = preference.fallback ? "The selected theme was unavailable, so a built-in theme is active." : `Using ${preference.resolved.name}.`;
      renderPasskeys(passkeys.passkeys || []);
      renderShares(shares.shares || []);
    } catch (error) {
      state.passkeys = [];
      state.shares = [];
      byID("passkey-list").replaceChildren(emptyTableRow("Passkeys unavailable", 4));
      byID("share-list").replaceChildren(emptyTableRow("Shares unavailable", 6));
      finishListLoading("passkey-list");
      finishListLoading("share-list");
      announce(friendlyError(error, "Settings could not be loaded."), true);
    }
  }

  function applyTheme(selection) {
    const link = byID("theme-stylesheet");
    if (!selection || !selection.cssURL) {
      link.disabled = true;
      link.removeAttribute("href");
      state.themeAssets = {};
      delete document.documentElement.dataset.theme;
      delete document.documentElement.dataset.appearance;
      document.querySelectorAll(".brand-mark, .entry-mark").forEach((image) => { image.src = "/assets/brand/endlessfs-mark.svg"; delete image.dataset.themeMedia; });
      document.querySelector("link[rel='icon']").href = "/assets/brand/endlessfs-mark.svg";
      return;
    }
    link.href = selection.cssURL;
    link.disabled = false;
    state.themeAssets = selection.assets || {};
    document.documentElement.dataset.theme = selection.resolved.id;
    document.documentElement.dataset.appearance = selection.resolved.appearance;
    const mark = state.themeAssets["brand.mark"];
    const favicon = state.themeAssets["brand.favicon"];
    if (mark && mark.url) {
      document.querySelectorAll(".brand-mark, .entry-mark").forEach((image) => {
        image.src = mark.url;
        image.dataset.themeMedia = "true";
        if (mark.fallbackURL && mark.fallbackURL !== mark.url) image.addEventListener("error", () => { image.src = mark.fallbackURL; }, { once: true });
      });
    }
    if (favicon && favicon.url) document.querySelector("link[rel='icon']").href = favicon.url;
    if (state.user && !byID("drive-view").hidden) renderFiles();
  }

  function renderPasskeys(passkeys) {
    if (passkeys) state.passkeys = passkeys;
    renderVirtualSettingsTable("passkey-table-scroll", "passkey-list", state.passkeys, 4, (passkey) => {
      const row = document.createElement("tr");
      const label = document.createElement("td");
      label.append(text("strong", passkey.label || "Passkey"));
      const added = text("td", formatDate(passkey.createdAt));
      const lastUsed = text("td", formatDate(passkey.lastUsedAt));
      const actions = document.createElement("td");
      actions.className = "table-icon-action-cell";
      const controls = document.createElement("div");
      controls.className = "row-actions";
      controls.append(iconButton("key-off", `Remove ${passkey.label || "passkey"}`, () => removePasskey(passkey), "danger", "Remove passkey"));
      actions.append(controls);
      row.append(label, added, lastUsed, actions);
      return row;
    }, "No passkeys", passkeys !== undefined);
  }

  function renderShares(shares) {
    if (shares) state.shares = shares;
    renderVirtualSettingsTable("share-table-scroll", "share-list", state.shares, 6, (share) => {
      const row = document.createElement("tr");
      const status = share.revokedAt ? "Revoked" : share.expiresAt && new Date(share.expiresAt) <= new Date() ? "Expired" : "Active";
      const location = document.createElement("td");
      location.append(text("strong", share.rootPath));
      const statusCell = text("td", status);
      const kind = text("td", share.kind);
      const created = text("td", formatDate(share.createdAt));
      const expires = text("td", share.expiresAt ? formatDate(share.expiresAt) : "—");
      const actions = document.createElement("td");
      if (status === "Active") {
        actions.className = "table-icon-action-cell";
        const controls = document.createElement("div");
        controls.className = "row-actions";
        controls.append(iconButton("link-off", `Revoke share for ${share.rootPath}`, () => revokeShare(share), "danger", "Revoke share"));
        actions.append(controls);
      }
      row.append(location, statusCell, kind, created, expires, actions);
      return row;
    }, "No public shares", shares !== undefined);
  }

  async function revokeShare(share) {
    const confirmed = await ask({ title: `Revoke share for ${share.rootPath}?`, description: "Anyone using the existing link will immediately see the same generic unavailable state.", confirm: "Revoke Share", danger: true });
    if (!confirmed) return;
    try { await api(`/api/v1/shares/${encodeURIComponent(share.shareID)}`, { method: "DELETE", body: {} }); announce(`Share for ${share.rootPath} revoked.`); await loadSettings(); }
    catch (error) { announce(friendlyError(error), true); }
  }

  async function removePasskey(passkey) {
    const confirmed = await ask({ title: `Remove ${passkey.label || "this passkey"}?`, description: "A recently authenticated session is required, and your final passkey cannot be removed.", confirm: "Remove Passkey", danger: true });
    if (!confirmed) return;
    try { await api(`/api/v1/me/passkeys/${encodeURIComponent(passkey.credentialID)}`, { method: "DELETE" }); announce("Passkey removed."); await loadSettings(); }
    catch (error) { announce(friendlyError(error), true); }
  }

  async function loadAdmin() {
    state.invites = [];
    state.users = [];
    state.usersCursor = "";
    state.usersLoading = true;
    byID("invite-table-scroll").scrollTop = 0;
    byID("users-table-scroll").scrollTop = 0;
    renderTableLoadingRows("invite-list", "invite", 4, 3);
    renderTableLoadingRows("user-list", "user", 5, 3);
    try {
      const [invites, users] = await Promise.all([api("/api/v1/admin/invites"), api("/api/v1/admin/users?limit=100")]);
      renderInvites(invites.invites || []);
      state.users = users.users || [];
      state.usersCursor = users.nextCursor || "";
      renderUsers(state.users, true);
      syncUsersPagingSentinel();
    } catch (error) {
      state.invites = [];
      byID("invite-list").replaceChildren(emptyTableRow("Invitations unavailable", 4));
      byID("user-list").replaceChildren();
      finishListLoading("invite-list");
      finishListLoading("user-list");
      announce(friendlyError(error, "Administration could not be loaded."), true);
    } finally {
      state.usersLoading = false;
    }
  }

  function renderInvites(invites) {
    if (invites) state.invites = invites;
    renderVirtualSettingsTable("invite-table-scroll", "invite-list", state.invites, 4, (invite) => {
      const row = document.createElement("tr");
      const status = invite.revokedAt ? "Revoked" : invite.usedAt ? "Used" : invite.expiresAt && new Date(invite.expiresAt) <= new Date() ? "Expired" : "Available";
      const statusCell = text("td", status);
      const created = text("td", formatDate(invite.createdAt));
      const expires = text("td", invite.expiresAt ? formatDate(invite.expiresAt) : "—");
      const actionCell = document.createElement("td");
      actionCell.className = "table-icon-action-cell";
      if (status === "Available") {
        const actions = document.createElement("div");
        actions.className = "row-actions";
        actions.append(iconButton("link-off", "Revoke invite", () => revokeInvite(invite), "danger", "Revoke invite"));
        actionCell.append(actions);
      }
      row.append(statusCell, created, expires, actionCell);
      return row;
    }, "No invitations", invites !== undefined);
  }

  function renderUsers(users, force = false) {
    if (users) state.users = users;
    renderVirtualSettingsTable("users-table-scroll", "user-list", state.users, 5, (user) => {
      const row = document.createElement("tr");
      row.append(text("td", user.displayName), text("td", user.status), text("td", user.admin ? "Administrator" : "Member"), text("td", formatDate(user.createdAt)));
      const actionCell = document.createElement("td"); actionCell.className = "table-icon-action-cell"; const actions = document.createElement("div"); actions.className = "user-actions";
      actions.append(iconButton(user.status === "enabled" ? "user-off" : "user-check", `${user.status === "enabled" ? "Disable" : "Enable"} ${user.displayName}`, () => adminUserAction(user, user.status === "enabled" ? "disable" : "enable"), `admin-row-action${user.status === "enabled" ? " danger" : ""}`, user.status === "enabled" ? "Disable user" : "Enable user"));
      actions.append(iconButton(user.admin ? "shield-minus" : "shield-plus", user.admin ? `Remove administrator access from ${user.displayName}` : `Make ${user.displayName} an administrator`, () => adminUserAction(user, "admin", user.admin ? "DELETE" : "POST"), `admin-row-action${user.admin ? " danger" : ""}`, user.admin ? "Remove administrator" : "Make administrator"));
      actions.append(iconButton("key", `Create recovery link for ${user.displayName}`, () => createRecovery(user), "admin-row-action", "Create recovery link")); actionCell.append(actions); row.append(actionCell);
      return row;
    }, "No users", force);
  }

  function syncUsersPagingSentinel() {
    byID("users-page-sentinel").hidden = !state.usersCursor;
  }

  async function loadMoreUsers() {
    if (!state.usersCursor || state.usersLoading) return;
    state.usersLoading = true;
    try {
      const page = await api(`/api/v1/admin/users?limit=100&cursor=${encodeURIComponent(state.usersCursor)}`);
      state.users = state.users.concat(page.users || []);
      state.usersCursor = page.nextCursor || "";
      renderUsers(state.users, true);
      syncUsersPagingSentinel();
    } catch (error) {
      announce(friendlyError(error, "More users could not be loaded."), true);
    } finally {
      state.usersLoading = false;
    }
  }

  async function adminUserAction(user, action, method = "POST") {
    const removingAdministrator = action === "admin" && method === "DELETE";
    const title = action === "disable" ? `Disable ${user.displayName}?` :
      action === "enable" ? `Enable ${user.displayName}?` :
      removingAdministrator ? `Remove administrator access from ${user.displayName}?` :
      `Make ${user.displayName} an administrator?`;
    const confirm = action === "disable" ? "Disable" : action === "enable" ? "Enable" : removingAdministrator ? "Remove Administrator" : "Make Administrator";
    const description = action === "disable" ? "This immediately invalidates the user’s active sessions." : action === "admin" ? "Administrator access changes instance-wide controls." : "This changes the user’s account access.";
    const confirmed = await ask({ title, description, confirm, danger: action === "disable" || removingAdministrator });
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
    if (append && state.publicLoading) return;
    syncFileBrowserAccess("public");
    showOnly("public-view");
    state.publicLoading = true;
    if (!append) {
      cleanupGridMedia(new Set());
      state.previewStates.clear();
      state.currentEntry = null;
      renderPathAggregate(null);
      state.entries = [];
      state.publicCursor = "";
      byID("list-presentation").scrollTop = 0;
      byID("media-grid").scrollTop = 0;
      showState("drive-state", "");
      renderFileLoadingItems();
    }
    const query = new URLSearchParams({ path: state.publicPath, limit: "100" });
    if (append && state.publicCursor) query.set("cursor", state.publicCursor);
    try {
      const page = await api(`/api/v1/public/shares/${encodeURIComponent(state.publicToken)}?${query.toString()}`);
      byID("browser-title").textContent = page.root.name || "Shared files";
      renderBreadcrumbs("breadcrumbs", state.publicPath, (path) => { state.publicPath = path; loadPublicShare(); });
      state.currentEntry = page.current;
      renderPathAggregate(page.current);
      const entries = page.entries && page.entries.length ? page.entries : (page.root.kind === "file" ? [page.root] : []);
      state.entries = append ? state.entries.concat(entries) : entries;
      state.publicCursor = page.nextCursor || "";
      renderFiles();
    } catch (error) {
      const message = error instanceof APIError && [403, 404, 410].includes(error.status) ? "This share is unavailable. It may have expired, moved, entered trash, or been revoked." : friendlyError(error, "The share could not be loaded.");
      if (!append) {
        state.currentEntry = null;
        renderPathAggregate(null);
        state.entries = [];
        byID("file-rows").replaceChildren();
        byID("media-grid-content").replaceChildren();
        finishListLoading("file-rows");
        finishListLoading("media-grid-content");
      }
      showState("drive-state", message, "error");
    } finally {
      state.publicLoading = false;
      requestAnimationFrame(() => maybeLoadNextBrowserPage(state.viewMode === "grid" ? byID("media-grid") : byID("list-presentation")));
    }
  }
