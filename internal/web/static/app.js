"use strict";

window.addEventListener("DOMContentLoaded", async () => {
  const build = document.querySelector("#build-version");
  if (!(build instanceof HTMLElement)) return;

  try {
    const response = await fetch("/api/v1/config", {
      credentials: "same-origin",
      headers: { Accept: "application/json" },
    });
    if (!response.ok) throw new Error("configuration unavailable");
    const config = await response.json();
    build.textContent = String(config.version || "development");
  } catch {
    build.textContent = "Unavailable";
  }
});
