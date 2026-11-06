// 仅改善长请求期间的可操作性；输入校验、会话、发送与保存规则全部由真实Go处理器执行。
// 不重试POST，不通过页面关闭或网络取消来声称硬件操作已撤销。
(() => {
  if (!window.fetch || !window.URLSearchParams) return;
  let pending = false;
  const operation = document.getElementById("devices-operation");
  let operationKey = "";
  const renderOperation = () => {
    if (!operationKey) return;
    operation.setAttribute("data-i18n", operationKey);
    operation.textContent = window.desktopI18n.text(operationKey);
  };
  // 共享适配器处理desktop:language后发出此通知；只更新等待文字，不提交或
  // 重建表单。原有请求完成后的只读跳转仍由原流程负责，语言变化不触发跳转。
  window.addEventListener("desktop:translated", renderOperation);
  document.addEventListener("submit", async (event) => {
    const form = event.target;
    if (!(form instanceof HTMLFormElement)) return;
    const path = new URL(form.action).pathname;
    if (path !== "/devices/trial/step" && path !== "/devices/trial/stop") return;
    if (pending && path !== "/devices/trial/stop") {
      event.preventDefault();
      return;
    }
    event.preventDefault();
    const body = new URLSearchParams(new FormData(form));
    pending = true;
    operation.hidden = false;
    operationKey = path.endsWith("/stop") ? "devices.operation_stop" : "devices.operation_wait";
    renderOperation();
    form.querySelectorAll("button").forEach((button) => { button.disabled = true; });
    try {
      const response = await fetch(form.action, { method: "POST", body, cache: "no-store" });
      if (!response.ok) {
        // 错误页包含真实处理器保留的未知状态；只重新读取，绝不重发当前请求。
        operationKey = "devices.operation_failed";
        renderOperation();
      }
      window.location.replace("/devices/");
    } catch {
      operationKey = "devices.operation_interrupted";
      renderOperation();
    }
  });
})();
