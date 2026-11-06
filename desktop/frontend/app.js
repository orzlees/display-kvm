"use strict";

const themeMode = document.getElementById("theme-mode");
const themeDescription = document.getElementById("theme-description");
const refreshButton = document.getElementById("refresh-status");
const refreshLabel = document.getElementById("refresh-label");
const statusMessage = document.getElementById("status-message");
const statusError = document.getElementById("status-error");
const connectionHeading = document.getElementById("connection-heading");
const devicePanel = document.querySelector(".device-panel");
const connectButton = document.getElementById("connect-backend");
const switchForm = document.getElementById("switch-form");
const switchButton = document.getElementById("switch-button");
const scopeKind = document.getElementById("scope-kind");
const scopeChoice = document.getElementById("scope-choice");
const targetChoice = document.getElementById("target-choice");
const displayList = document.getElementById("display-list");
const resultList = document.getElementById("result-list");

let lightQuery = null;
let darkQuery = null;
let requestController = null;
let requestSerial = 0;
let pollTimer = null;
let state = null;
let acting = false;
let needsStatus = false;
let leaving = false;
let languageDirty = false;
const t = (key, values) => window.desktopI18n.text(key, values);
const label = (element, key, values = {}) => {
  if (t(key,values)===key) return;
  element.dataset.i18n = key;
  element.dataset.i18nValues = JSON.stringify(values);
  element.textContent = t(key, values);
};

try {
  if (typeof window.matchMedia === "function") {
    lightQuery = window.matchMedia("(prefers-color-scheme: light)");
    darkQuery = window.matchMedia("(prefers-color-scheme: dark)");
  }
} catch (error) {
  // 系统外观读取不是连接能力；失败时只回退深色并说明原因，不能阻塞窗口。
  lightQuery = null;
  darkQuery = null;
  console.warn("读取系统外观能力失败，将使用深色兜底。", error);
}

function applyTheme() {
  // 两个查询均不匹配（或同时匹配）时无法确认系统外观，保留深色兜底。
  // 只在内存保存本次选择，不新增核心配置或持久化偏好文件。
  const isLight = lightQuery?.matches === true;
  const isDark = darkQuery?.matches === true;
  const systemTheme = isLight !== isDark ? (isLight ? "light" : "dark") : null;
  const mode = themeMode.value;
  document.documentElement.dataset.theme = mode === "system" ? (systemTheme ?? "dark") : mode;
  label(themeDescription, mode === "system" ? (systemTheme ? "overview.theme_system" : "overview.theme_unread") : (mode === "dark" ? "overview.theme_dark" : "overview.theme_light"));
}

themeMode.addEventListener("change", () => {
  applyTheme();
  send("appearance", { mode: themeMode.value });
});
for (const query of [lightQuery, darkQuery]) {
  // 部分 WebKit 只提供旧的监听入口；复用同一回调，主题变化仅改 CSS，
  // 不重建页面、读取设备、重连后台或重新发送任何操作。
  if (typeof query?.addEventListener === "function") {
    query.addEventListener("change", applyTheme);
  } else if (typeof query?.addListener === "function") {
    query.addListener(applyTheme);
  }
}
applyTheme();

function text(id, value) { document.getElementById(id).textContent = value; }

function updateOptions(select, options, value, placeholder) {
  const items = [new Option(placeholder, ""), ...options.map((option) => new Option(option.name, option.id))];
  select.replaceChildren(...items);
  select.value = value;
}

function updateControls() {
  const unavailable = acting || !state?.connected || state?.connecting || state?.busy;
  for (const select of [scopeKind, scopeChoice, targetChoice]) select.disabled = unavailable;
  switchButton.disabled = acting || needsStatus || !state?.canSwitch;
  refreshButton.disabled = acting || state?.busy || state?.connecting;
  connectButton.disabled = acting || state?.busy || state?.connecting;
  themeMode.disabled = acting;
  for (const button of resultList.querySelectorAll("button")) {
    button.disabled = acting || state?.busy || button.dataset.confirmed === "true";
  }
  label(refreshLabel, state?.connecting ? "overview.refreshing" : "overview.refresh");
}

// 复用有逻辑ID的行节点，状态刷新时不替换正在使用的人工确认按钮，保留键盘焦点。
// 所有后台名称和错误都以textContent呈现，不将配置内容拼成HTML。
function updateRows(list, templateID, rows, key, update) {
  const existing = new Map([...list.children].map((row) => [row.dataset.id, row]));
  rows.forEach((data, index) => {
    const id = data[key];
    const row = existing.get(id) ?? document.getElementById(templateID).content.firstElementChild.cloneNode(true);
    row.dataset.id = id;
    update(row, data);
    if (list.children[index] !== row) list.insertBefore(row, list.children[index] ?? null);
    existing.delete(id);
  });
  for (const row of existing.values()) row.remove();
}

function timeLabel(value) {
  if (!value) return "";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toLocaleTimeString(window.desktopI18n.snapshot().locale, { hour12: false });
}

function render(view, force = false, languageOnly = false) {
  if(!window.desktopI18n.snapshot().messages["overview.switch"]) return;
  const focusedResult=languageOnly && document.activeElement?.matches(".confirm-button") ? {display:document.activeElement.dataset.displayID,request:document.activeElement.dataset.requestID,left:window.scrollX,top:window.scrollY} : null;
  if (state && view.revision < state.revision) return;
  const changed = force || !state || view.revision !== state.revision;
  state = view;
  if (changed) {
    connectionHeading.removeAttribute("data-i18n");
    connectionHeading.textContent = view.connectionMessage;
    label(document.getElementById("connection-state"),view.connecting ? "overview.connecting" : (view.connected ? "overview.connected" : "overview.disconnected"));
    const strip = document.querySelector(".connection-strip");
    strip.dataset.connected = String(view.connected);
    label(document.getElementById("connection-description"), view.connected ? "overview.connected_hint" : "overview.reconnect_hint");
    label(connectButton,view.connected || view.generation > 0 ? "overview.reconnect" : "overview.connect");
    if (!languageOnly) scopeKind.value = view.selection.kind;
    const isGroup = view.selection.kind === "group";
    label(document.getElementById("scope-choice-label"), isGroup ? "overview.group" : "overview.display");
    if (!languageOnly) {
      updateOptions(scopeChoice, isGroup ? view.groups : view.displays, isGroup ? view.selection.groupID : view.selection.displayID, t(isGroup ? "overview.choose_group" : "overview.choose_display"));
      updateOptions(targetChoice, view.targets, view.selection.targetID, t("overview.choose_target"));
    } else {
      if (scopeChoice.options[0]) scopeChoice.options[0].textContent=t(isGroup ? "overview.choose_group" : "overview.choose_display");
      if (targetChoice.options[0]) targetChoice.options[0].textContent=t("overview.choose_target");
    }
    const selectionHint=document.getElementById("selection-hint");
    selectionHint.removeAttribute("data-i18n");
    if(view.blockedReason) selectionHint.textContent=view.blockedReason; else label(selectionHint,"overview.only_scope_hint");
    label(switchButton,view.busy ? "overview.waiting" : "overview.switch");
    switchForm.setAttribute("aria-busy", String(view.busy));
    devicePanel.setAttribute("aria-busy", String(view.connecting));
    label(document.getElementById("devices-caption"),view.connected ? "overview.display_count" : (view.displays.length ? "overview.last_read" : "app.display.unread"),{count:view.displays.length});
    document.getElementById("empty-state").hidden = view.displays.length > 0;
    label(document.getElementById("empty-heading"),view.connected ? "overview.empty_connected" : "overview.empty_disconnected");
    label(document.getElementById("empty-description"),view.connected ? "overview.empty_setup_hint" : "overview.empty_connect_hint");
    label(document.getElementById("device-note"),view.unconfiguredCount > 0 ? "overview.unconfigured_hint" : "overview.refresh_hint",{count:view.unconfiguredCount});
    updateRows(displayList, "display-row", view.displays, "id", (row, display) => {
      row.querySelector(".display-name").textContent = display.name;
      row.querySelector(".display-reason").textContent = display.reason;
      label(row.querySelector(".row-tag"),display.selected ? "overview.selected" : (display.available ? "overview.available" : "overview.unavailable"));
      row.dataset.selected = String(display.selected);
      row.dataset.available = String(display.available);
    });
    document.getElementById("results-panel").hidden = view.results.length === 0;
    label(document.getElementById("attempt-target"),"overview.last_target",{name:view.attemptTarget});
    text("result-summary", view.summary);
    updateRows(resultList, "result-row", view.results, "displayID", (row, result) => {
      row.dataset.tone = result.tone;
      row.querySelector(".result-name").textContent = result.name;
      row.querySelector(".result-label").textContent = result.label;
      row.querySelector(".result-detail").textContent = result.detail;
      if(result.diagnostic) row.querySelector(".result-detail").textContent += ` · ${t("common.original_detail")}: ${result.diagnostic.detail || result.diagnostic.message}`;
      const evidence = [];
      if (result.sentAt) evidence.push(t("overview.sent_time",{time:timeLabel(result.sentAt)}));
      if (result.observation?.observed_at) evidence.push(t("overview.observed_time",{time:timeLabel(result.observation.observed_at)}));
      row.querySelector(".result-evidence").textContent = evidence.join(" · ");
      const button = row.querySelector(".confirm-button");
      button.dataset.confirmed = String(result.manualConfirmed);
      button.dataset.displayID = result.displayID;
      button.dataset.requestID = view.requestID;
      label(button,result.manualConfirmed ? "overview.picture_confirmed" : "overview.confirm_picture");
      button.setAttribute("aria-label", `${result.name}：${button.textContent}`);
      button.hidden = result.state === "waiting";
    });
    statusMessage.removeAttribute("data-i18n");
    statusMessage.textContent = view.message;
    document.getElementById("tray-message").removeAttribute("data-i18n");
    text("tray-message", view.trayMessage);
    if (!languageOnly) themeMode.value = view.appearance;
    applyTheme();
    if(focusedResult){
      const button=[...resultList.querySelectorAll(".confirm-button")].find((item)=>item.dataset.displayID===focusedResult.display && item.dataset.requestID===focusedResult.request);
      button?.focus({preventScroll:true});
      window.scrollTo(focusedResult.left,focusedResult.top);
    }
  }
  updateControls();
}

async function send(action = "status", body = null) {
  if (leaving || (action === "status" && requestController) || (action !== "status" && acting)) return;
  // 主动操作可替换未完成的只读轮询；POST从不自动重试。即使本地读取中断，
  // 后台动作仍可能已受理，下一次只读取共享状态，不能把超时解释为未发送。
  if (action !== "status") {
    requestController?.abort();
    acting = true;
    statusError.hidden = true;
    updateControls();
  }
  window.clearTimeout(pollTimer);
  const controller = new AbortController();
  requestController = controller;
  const serial = ++requestSerial;
  const timeout = window.setTimeout(() => controller.abort(), action === "connect" || action === "refresh" ? 15000 : 8000);
  try {
    const response = await fetch(`/client/${action}`, {
      method: action === "status" ? "GET" : "POST",
      headers: { Accept: "application/json", "Content-Type": "application/json" },
      body: action === "status" ? null : JSON.stringify(body ?? {}),
      cache: "no-store",
      signal: controller.signal,
    });
    const payload = await response.json();
    if (serial !== requestSerial) return;
    if (!payload.view || !Array.isArray(payload.view.displays) || !Array.isArray(payload.view.results)) {
      throw new Error(t("overview.incomplete_state"));
    }
    if (action === "status") needsStatus = false;
    // 首次词典到达也会发语言事件，但尚无可保留的选择；初次状态仍须完整初始化。
    // 只读语言刷新不改本地选择，正常操作响应继续沿用既有状态同步流程。
    render(payload.view, action !== "status" || languageDirty, action === "status" && languageDirty && state !== null);
    languageDirty=false;
    if (!response.ok) {
      statusError.removeAttribute("data-i18n");
      statusError.textContent = payload.error || t("overview.operation_error");
      if(payload.diagnostic?.detail) statusError.textContent += ` · ${t("common.original_detail")}: ${payload.diagnostic.detail}`;
      statusError.hidden = false;
    } else if (action !== "status" || !needsStatus) {
      statusError.hidden = true;
    }
  } catch (error) {
    if (serial !== requestSerial || leaving) return;
    console.error("读取桌面状态失败。", error);
    if (action !== "status") needsStatus = true;
    label(statusError,action === "switch" ? "overview.switch_unknown" : "overview.state_error");
    statusError.hidden = false;
  } finally {
    window.clearTimeout(timeout);
    if (serial === requestSerial) {
      requestController = null;
      acting = false;
      updateControls();
      if (!leaving) pollTimer = window.setTimeout(() => send("status"), state?.busy || state?.connecting || needsStatus ? 700 : 2500);
    }
  }
}

function formSelection(resetScope = false) {
  const isGroup = scopeKind.value === "group";
  return {
    kind: scopeKind.value,
    displayID: !isGroup && !resetScope ? scopeChoice.value : "",
    groupID: isGroup && !resetScope ? scopeChoice.value : "",
    targetID: targetChoice.value,
    generation: state?.generation ?? 0,
  };
}

scopeKind.addEventListener("change", () => send("selection", formSelection(true)));
scopeChoice.addEventListener("change", () => send("selection", formSelection()));
targetChoice.addEventListener("change", () => send("selection", formSelection()));
switchForm.addEventListener("submit", (event) => {
  event.preventDefault();
  if (!switchButton.disabled) send("switch", formSelection());
});
refreshButton.addEventListener("click", () => send(state?.connected ? "refresh" : "status"));
connectButton.addEventListener("click", () => send("connect"));
resultList.addEventListener("click", (event) => {
  const button = event.target.closest(".confirm-button");
  if (button && !button.disabled) send("confirm", { requestID: button.dataset.requestID, displayID: button.dataset.displayID });
});
window.addEventListener("pagehide", () => {
  leaving = true;
  window.clearTimeout(pollTimer);
  requestController?.abort();
});
send("status");
window.addEventListener("desktop:translated",()=>{
  languageDirty=true;
  applyTheme();
  updateControls();
  // 只读状态带回Service使用同一新快照投影的动态消息；不重连、不重发POST。
  send("status");
});
