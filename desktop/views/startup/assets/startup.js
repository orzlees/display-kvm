(() => {
  "use strict";
  const page = document.querySelector(".startup-page");
  let form = document.getElementById("startup-form");
  if (!page || !form) return;

  let changed = page.dataset.dirty === "true";
  let submitting = false;
  const t=(key)=>window.desktopI18n.text(key);
  const markChanged = () => {
    changed = true;
    const status=document.getElementById("startup-draft-state");
    status.dataset.i18n="common.pending";
    status.textContent = t("common.pending");
  };
  // 文件操作后使用服务端新表单替换内容；事件留在不替换的页面容器上，
  // 避免丢失后续编辑、提交和防重复处理，也不重复注册窗口级监听。
  page.addEventListener("input", markChanged);
  page.addEventListener("change", markChanged);
  page.addEventListener("keydown", (event) => {
    // 输入法确认和路径输入中的Enter不应触发表单第一个按钮或提前保存。
    if (event.key === "Enter" && (event.isComposing || event.keyCode === 229 || event.target.matches('input[type="text"]'))) {
      event.preventDefault();
    }
  });
  const disableControls = () => {
    form.querySelectorAll("button, input, select").forEach((control) => { control.disabled = true; });
    form.setAttribute("aria-busy", "true");
  };
  const updateInPlace = async (action, position) => {
    try {
      // 先序列化成功控件，再禁用它们；已禁用的未知项仍不提交。
      // Wails的macOS原生资源请求支持字符串正文，避免直接发送FormData。
      const body = new URLSearchParams(new FormData(form));
      body.set("action", action);
      disableControls();
      // name=action的控件会遮蔽form.action属性，读取HTML特性才能取得请求地址。
      const response = await fetch(form.getAttribute("action"), {
        method: "POST",
        headers: {"Content-Type": "application/x-www-form-urlencoded"},
        body: body.toString(),
        cache: "no-store",
        redirect: "error",
      });
      // 来源/格式/旧版本错误可能尚未接收本次草稿，不能导入旧页面。
      // 整页保存的部分结果409仅在服务端确认已受理同一请求版本时导入，
      // 才能显示真实分项结果并保留本次草稿，而不是误报传输失败。
      const acceptedPartial = response.status === 409 && response.headers.get("X-Settings-Request-Revision") === body.get("revision");
      if (!response.ok && !acceptedPartial) throw new Error(t("settings.request_failed"));
      const documentResult = new DOMParser().parseFromString(await response.text(), "text/html");
      const nextPage = documentResult.querySelector(".startup-page");
      const nextForm = nextPage?.querySelector("#startup-form");
      if (!nextForm || nextForm.getAttribute("action") !== "/startup/" || !/^\d+$/.test(nextForm.querySelector('input[name="revision"]')?.value || "")) {
        throw new Error(t("settings.request_failed"));
      }
      // 文件及选择确认后，同次响应已经带新Catalog；不等待原生事件到达，
      // 避免新增文字被旧语言快照重新覆盖。事件仍只更新呈现，不提交草稿。
      const nextCatalog=documentResult.getElementById("desktop-catalog");
      if(nextCatalog) window.dispatchEvent(new CustomEvent("desktop:language",{detail:JSON.parse(nextCatalog.textContent)}));
      // 已受理的响应（包括部分保存、文件错误和取消）采用服务端草稿与新版本。
      // 只保留旧DOM或返回204会留下旧版本，导致下一次选择或保存失效。
      page.replaceChildren(...document.importNode(nextPage, true).childNodes);
      page.dataset.dirty = nextPage.dataset.dirty;
      form = document.getElementById("startup-form");
      changed = page.dataset.dirty === "true";
      // 保持同一文档，无需浏览器存储或跨页恢复记录；文件动作恢复原位置。
      // preventScroll避免恢复按钮焦点时先跳动，随后按原坐标修正内容变化的偏移。
      window.requestAnimationFrame(() => {
        const button = [...form.querySelectorAll('button[name="action"]')].find((control) => control.value === action && !control.disabled)
          || form.querySelector('button[value="save"]:not(:disabled)')
          || form.querySelector('button[value="reload_file"]:not(:disabled)');
        // 页面顶部的反馈可能在当前视口之外；把纯文本反馈同时放在发起位置，
        // 保持滚动与按钮焦点时也能知道取消、保存成功或文件错误的具体原因。
        const feedback = page.querySelector("#startup-error") || page.querySelector(".startup-notice");
        if (feedback && button) {
          const local = document.createElement("p");
          local.className = feedback.id === "startup-error" ? "startup-field-error" : "startup-detail";
          local.setAttribute("role", feedback.id === "startup-error" ? "alert" : "status");
          // 复制已转义的纯文案节点及key，后续语言事件仍能更新局部反馈。
          local.append(...[...feedback.childNodes].map((node)=>node.cloneNode(true)));
          button.closest(".startup-path, .startup-panel, .startup-footer")?.append(local);
        }
        button?.focus({preventScroll: true});
        window.scrollTo(position.left, position.top);
      });
    } catch {
      // 请求可能已消耗表单版本，不能自动重发或继续使用旧表单；保留当前DOM
      // 中的草稿供核对，只有明确重新读取才能恢复操作，不推断选择已取消。
      const progress = document.getElementById("startup-progress");
      const reload = document.createElement("a");
      reload.href = "/startup/";
      reload.dataset.i18n="settings.read_link";
      reload.textContent = t("settings.read_link");
      const message=document.createElement("span");
      message.dataset.i18n="settings.request_failed";
      message.textContent=t("settings.request_failed");
      progress.removeAttribute("data-i18n");
      progress.replaceChildren(message," ", reload);
      disableControls();
      form.removeAttribute("aria-busy");
      const button = [...form.querySelectorAll('button[name="action"]')].find((control) => control.value === action);
      button?.closest(".startup-path, .startup-panel, .startup-footer")?.append(progress);
    } finally {
      submitting = false;
    }
  };
  page.addEventListener("submit", (event) => {
    if (submitting) {
      event.preventDefault();
      return;
    }
    const action = event.submitter?.value;
    if (!action) {
      event.preventDefault();
      return;
    }
    if (["cancel", "reload", "reload_file", "new_config", "choose_config"].includes(action) && changed && !window.confirm(t("settings.confirm.discard"))) {
      event.preventDefault();
      return;
    }
    const inPlace = action !== "cancel" && action !== "reload" && window.fetch && window.FormData && window.URLSearchParams && window.DOMParser;
    const position = {left: window.scrollX, top: window.scrollY};
    submitting = true;
    const progress = document.getElementById("startup-progress");
    progress.hidden = false;
    progress.dataset.i18n="common.processing";
    progress.textContent = t("common.processing");
    if (inPlace) {
      event.preventDefault();
      void updateInPlace(action, position);
      return;
    }
    // 浏览器先序列化当前按钮和字段，再禁用控件。过早disabled会漏传开关，
    // 把勾选项误当关闭；服务端版本检查仍负责拦截旧表单的重复请求。
    window.setTimeout(disableControls, 0);
  });
  window.addEventListener("beforeunload", (event) => {
    if (changed && !submitting) {
      event.preventDefault();
      event.returnValue = "";
    }
  });
  // 浏览器恢复历史页面时也要恢复交互；保存是否重复仍由服务端表单版本判断。
  window.addEventListener("pageshow", (event) => {
    if (event.persisted) window.location.reload();
  });
  document.getElementById("startup-error")?.focus();
})();
