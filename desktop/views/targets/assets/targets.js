(() => {
  const page = document.querySelector(".targets-page");
  const editor = document.querySelector("#targets-editor-form");
  if (!page) return;

  let changed = false;
  let submitting = false;
  editor?.addEventListener("input", () => { changed = true; updatePreviewControls(); });
  editor?.addEventListener("change", () => { changed = true; updatePreviewControls(); });

  // 组合输入期间的Enter属于输入法确认，不能同时触发表单保存。
  // 保存、校验及运行代判断仍在Go处理器完成，此处只保护键盘交互。
  editor?.addEventListener("keydown", (event) => {
    if (event.key === "Enter" && (event.isComposing || event.keyCode === 229)) event.preventDefault();
  });

  document.querySelectorAll("form").forEach((form) => {
    form.addEventListener("submit", (event) => {
      if (form.matches("[data-preview-start], [data-preview-query]")) return;
      const button = event.submitter;
      if (button?.matches("[data-remove]") && !window.confirm(window.desktopI18n.text("targets.confirm_remove"))) {
        event.preventDefault();
        return;
      }
      if (button?.matches("[data-cancel]") && (changed || page.dataset.dirty === "true") && !window.confirm(window.desktopI18n.text("targets.confirm_cancel"))) {
        event.preventDefault();
        return;
      }
      if (form.matches("[data-leave-editor]") && changed && !window.confirm(window.desktopI18n.text("targets.confirm_leave"))) {
        event.preventDefault();
        return;
      }
      if (form.matches("[data-reload]") && (changed || page.dataset.dirty === "true") && !window.confirm(window.desktopI18n.text("targets.confirm_reload"))) {
        event.preventDefault();
        return;
      }
      submitting = true;
    });
  });

  window.addEventListener("beforeunload", (event) => {
    if (!submitting && (changed || page.dataset.dirty === "true")) {
      event.preventDefault();
      event.returnValue = "";
    }
  });
  document.querySelector("#targets-error")?.focus();

  let previewRequest = false;
  let previewUnknown = false;
  let previewTimer;
  const previewPanel = () => document.querySelector("#input-preview");
  const text = (key, values = {}) => window.desktopI18n.text(key, values);

  function updatePreviewControls() {
    const panel = previewPanel();
    if (!panel) return;
    const button = panel.querySelector("[data-preview-start] button");
    if (button) button.disabled = previewRequest || previewUnknown || changed || panel.dataset.canStart !== "true";
    if (changed && !previewUnknown && panel.dataset.unknown !== "true") {
      panel.querySelector("[data-preview-hint]").textContent = text("targets.preview.save_first");
    }
  }

  function renderCountdown() {
    const panel = previewPanel();
    const countdown = panel?.querySelector("[data-preview-countdown]");
    if (!countdown) return;
    const until = Date.parse(panel.dataset.holdUntil || "");
    countdown.hidden = previewUnknown || panel.dataset.unknown === "true" || !Number.isFinite(until);
    if (countdown.hidden) return;
    const seconds = Math.max(0, Math.ceil((until - Date.now()) / 1000));
    // hold_until来自后台。浏览器时钟和计时器只影响剩余时间的显示，
    // 即使归零也必须继续查询恢复证据，不能宣称已恢复或发送回切请求。
    countdown.textContent = seconds > 0 ? text("targets.preview.countdown", { seconds: String(seconds) }) : text("targets.preview.waiting_restore");
  }

  function showPreviewUnknown() {
    const panel = previewPanel();
    if (!panel) return;
    previewUnknown = true;
    panel.dataset.unknown = "true";
    panel.querySelector("[data-preview-state]").textContent = text("targets.preview.state.unknown");
    panel.querySelector("[data-preview-hint]").textContent = text("targets.preview.unknown_hint");
    const connection = panel.querySelector("[data-preview-connection]");
    connection.hidden = false;
    connection.textContent = text("targets.preview.last_evidence");
    updatePreviewControls();
    renderCountdown();
  }

  async function previewRequestPage(form) {
    if (previewRequest) return;
    const panel = previewPanel();
    if (!panel) return;
    clearTimeout(previewTimer);
    previewRequest = true;
    updatePreviewControls();
    const endpoint = new URL("/targets/", window.location.href);
    let options = { cache: "no-store" };
    if (form?.matches("[data-preview-start]")) {
      const values = new URLSearchParams(new FormData(form));
      values.set("action", "preview_start");
      options = { ...options, method: "POST", body: values };
    } else {
      endpoint.searchParams.set("preview_status", "1");
      endpoint.searchParams.set("preview_session", panel.dataset.session || "");
    }
    // 启动只发送一次；无论HTTP状态、网络中断还是解析失败，后续都只查状态。
    // 不整页替换，避免短轮询丢失编辑框中的未保存内容和当前焦点。
    const controller = new AbortController();
    const deadline = window.setTimeout(() => controller.abort(), 10000);
    try {
      const response = await fetch(endpoint, { ...options, signal: controller.signal });
      const documentResult = new DOMParser().parseFromString(await response.text(), "text/html");
      const replacement = documentResult.querySelector("#input-preview");
      if (!replacement || !response.ok) throw new Error("preview status unavailable");
      const focusedQuery = panel.querySelector("[data-preview-query] button") === document.activeElement;
      panel.replaceWith(document.importNode(replacement, true));
      // 只有POST会推进共享表单版本；同步隐藏字段，保留全部草稿输入值。
      const revision = documentResult.querySelector('input[name="revision"]')?.value;
      if (revision) document.querySelectorAll('input[name="revision"]').forEach((input) => { input.value = revision; });
      previewUnknown = false;
      const current = previewPanel();
      if (current.dataset.session) {
        const address = new URL(window.location.href);
        address.searchParams.set("preview_session", current.dataset.session);
        window.history.replaceState(null, "", address);
      }
      if (focusedQuery) current.querySelector("[data-preview-query] button")?.focus();
      // 页面响应可能比当前语言偏好更早生成，替换后重新消费共享词典。
      window.desktopI18n.render(current);
    } catch {
      showPreviewUnknown();
    } finally {
      clearTimeout(deadline);
      previewRequest = false;
      updatePreviewControls();
      renderCountdown();
      const current = previewPanel();
      if (current && (current.dataset.active === "true" || current.dataset.unknown === "true")) {
        previewTimer = window.setTimeout(() => previewRequestPage(), 1200);
      }
    }
  }

  document.addEventListener("submit", (event) => {
    const form = event.target;
    if (!form.matches("[data-preview-start], [data-preview-query]")) return;
    event.preventDefault();
    if (form.matches("[data-preview-start]") && (changed || previewUnknown || previewPanel()?.dataset.canStart !== "true")) return;
    previewRequestPage(form);
  });
  window.addEventListener("desktop:translated", () => {
    if (previewUnknown) showPreviewUnknown();
    updatePreviewControls();
    renderCountdown();
  });
  window.setInterval(renderCountdown, 250);
  // 每次打开页面先查询current/latest；恢复由后台继续，重载绝不重发Start。
  previewRequestPage();
})();
