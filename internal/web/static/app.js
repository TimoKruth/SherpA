(() => {
  "use strict";

  if (!navigator.clipboard || typeof navigator.clipboard.writeText !== "function") {
    return;
  }

  const buttons = document.querySelectorAll("[data-copy-button]");
  if (buttons.length === 0) {
    return;
  }

  document.documentElement.classList.add("clipboard-ready");
  const resetTimers = new WeakMap();

  for (const button of buttons) {
    const row = button.closest("[data-copy-row], .command-row");
    const commandSurface = button.previousElementSibling;
    const command = commandSurface?.matches("[data-copy-text], code")
      ? commandSurface
      : commandSurface?.querySelector("[data-copy-text], code");
    const status = row?.querySelector("[data-copy-status]");

    if (!row || !command || !status) {
      button.hidden = true;
      continue;
    }

    button.hidden = false;
    button.addEventListener("click", async () => {
      const value = command.textContent ?? "";
      if (value.length === 0) {
        status.textContent = "Nothing to copy.";
        return;
      }

      const existingTimer = resetTimers.get(button);
      if (existingTimer !== undefined) {
        window.clearTimeout(existingTimer);
      }

      try {
        await navigator.clipboard.writeText(value);
        button.dataset.copyState = "success";
        button.setAttribute("aria-label", "Copied command");
        status.textContent = "Command copied.";
        const timer = window.setTimeout(() => {
          delete button.dataset.copyState;
          button.setAttribute("aria-label", "Copy command");
          status.textContent = "";
          resetTimers.delete(button);
        }, 2000);
        resetTimers.set(button, timer);
      } catch {
        delete button.dataset.copyState;
        button.setAttribute("aria-label", "Copy command");
        status.textContent = "Copy failed. Select the command text instead.";
      }
    });
  }
})();
