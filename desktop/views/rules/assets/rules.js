(() => {
  "use strict";
  const byID = (id) => document.getElementById(id);
  const copy = (value) => JSON.parse(JSON.stringify(value));
  const t = (key, values = {}) => window.desktopI18n.text(key, values);
  const named = (prefix, value) => {
    const key = `${prefix}.${value}`;
    return Object.prototype.hasOwnProperty.call(window.desktopI18n.snapshot().messages, key) ? t(key) : value || "";
  };
  // 动态插值可能包含已翻译的事件名；保留投影而非文本快照。弱引用使已移除的列表节点可回收。
  const projections = new WeakMap();
  function label(node, key, values = {}) {
    const current = typeof values === "function" ? values() : values;
    node.dataset.i18n = key;
    node.dataset.i18nValues = JSON.stringify(current);
    node.textContent = t(key, current);
    if (typeof values === "function") projections.set(node, () => label(node, key, values));
    else projections.delete(node);
    return node;
  }
  function raw(node, value) {
    delete node.dataset.i18n;
    delete node.dataset.i18nValues;
    projections.delete(node);
    node.textContent = value || "";
  }
  const failure = (key) => Object.assign(new Error(""), {summary_key: key});
  function errorDetail(node, error) {
    const summary = document.createElement("span");
    label(summary, error.summary_key || (error.code ? `error.${error.code}` : "rules.error.operation"), error.summary_values || {});
    node.append(summary);
    if (error.message) {
      const detail = document.createElement("p");
      label(detail, "rules.original_detail", {detail: error.message});
      node.append(detail);
    }
  }
  let state = null;
  let selectedID = null;
  let original = null;
  let dirty = false;
  let busy = false;
  let commit = null;
  let draftGeneration = null;
  let draftRules = null;

  function announce(message, failed = false) {
    byID("error").hidden = !failed;
    raw(byID("error"), "");
    raw(byID("message"), "");
    if (typeof message === "string") label(byID(failed ? "error" : "message"), message);
    else errorDetail(byID(failed ? "error" : "message"), message);
  }
  function markDirty() {
    dirty = true;
    byID("draft-tag").hidden = false;
    label(byID("draft-detail"), "rules.draft_dirty");
  }
  function options(select, values, placeholder = "rules.choose") {
    select.replaceChildren(label(new Option("", ""), placeholder));
    for (const item of values || []) select.add(new Option(item.name || item.id, item.id));
  }
  function selectPreserving(select, value) {
    if (value && !Array.from(select.options).some((option) => option.value === value)) select.add(label(new Option("", value), "rules.unavailable_value", {value}));
    select.value = value || "";
  }
  function renderCapabilities() {
    for (const id of ["event-kind", "preview-kind"]) {
      const select = byID(id);
      select.replaceChildren(label(new Option("", ""), "rules.choose_event"));
      for (const capability of state.capabilities || []) {
        const option = label(new Option("", capability.kind), capability.available ? "rules.event_option" : "rules.event_unavailable", () => ({event: named("rules.event", capability.kind)}));
        option.disabled = !capability.available;
        select.add(option);
      }
    }
    byID("capability-list").replaceChildren();
    for (const capability of state.capabilities || []) {
      const row = document.createElement("li");
      row.dataset.available = String(capability.available);
      const title = document.createElement("span");
      label(title, capability.available ? "rules.capability_available" : "rules.capability_unavailable", () => ({event: named("rules.event", capability.kind)}));
      const detail = document.createElement("small");
      if (capability.reason) detail.append(document.createTextNode(capability.reason));
      else detail.append(label(document.createElement("span"), "rules.capability_provided"));
      if (capability.filter_fields?.length) detail.append(label(document.createElement("span"), "rules.filter_fields", {fields: capability.filter_fields.join(", ")}));
      row.append(title, detail);
      byID("capability-list").append(row);
    }
  }
  function renderRules() {
    byID("rule-count").textContent = String(state.rules.length);
    byID("empty-rules").hidden = state.rules.length !== 0;
    byID("rule-list").replaceChildren();
    for (const rule of state.rules) {
      const row = byID("rule-row").content.firstElementChild.cloneNode(true);
      row.dataset.selected = String(rule.id === selectedID);
      row.dataset.enabled = String(rule.enabled);
      row.querySelector(".rules-rule-id").textContent = rule.id;
      label(row.querySelector(".rules-rule-status"), rule.enabled ? "schedules.enabled_state" : "schedules.disabled_state");
      const target = (state.targets || []).find((item) => item.id === rule.action.target_id);
      label(row.querySelector(".rules-rule-summary"), "rules.rule_summary", () => ({
        trigger: rule.when.usb_present === undefined ? named("rules.event", rule.when.kind) : t(rule.when.usb_present ? "rules.usb_present" : "rules.usb_absent"),
        target: target?.name || rule.action.target_id,
        scope: rule.action.group_id ? t("targets.groups") : rule.action.display_ids?.length ? t("rules.display_count", {count: String(rule.action.display_ids.length)}) : t("rules.all_displays")
      }));
      const record = (state.records || []).slice().reverse().find((item) => item.decision.rule_id === rule.id);
      label(row.querySelector(".rules-rule-last"), record ? "rules.latest_result" : "rules.no_result", () => ({reason: record ? named("rules.reason", record.decision.reason) : ""}));
      row.querySelector(".rules-edit").addEventListener("click", () => openEditor(rule));
      const toggle = row.querySelector(".rules-toggle");
      label(toggle, rule.enabled ? "rules.disable_action" : "rules.enable_action");
      toggle.addEventListener("click", () => {
        if (openEditor(rule)) {
          byID("rule-enabled").checked = !rule.enabled;
          markDirty();
          announce("rules.notice.toggle");
        }
      });
      byID("rule-list").append(row);
    }
  }
  function conditionFields() {
    const capability = (state.capabilities || []).find((item) => item.kind === byID("event-kind").value);
    const isStateRule = original?.when.usb_present !== undefined;
    if (isStateRule) label(byID("event-reason"), "rules.state_rule_hint");
    else if (capability?.reason) raw(byID("event-reason"), capability.reason);
    else label(byID("event-reason"), "rules.event_capability_hint");
    byID("condition-notice").hidden = !isStateRule;
    label(byID("condition-notice"), "rules.state_condition_hint");
    byID("event-kind").disabled = isStateRule;
    for (const field of document.querySelectorAll("#rule-form [data-filter]")) {
      const name = field.dataset.filter;
      const allowed = !isStateRule && (capability?.filter_fields || []).some((value) => value === name || (name === "attributes" && value.startsWith("attributes.")));
      field.querySelector("input, textarea").disabled = !allowed;
      field.dataset.available = String(allowed);
    }
  }
  function scopeFields() {
    byID("group-field").hidden = byID("scope-kind").value !== "group";
    byID("displays-field").hidden = byID("scope-kind").value !== "displays";
  }
  function openEditor(rule) {
    if (busy || commit) { announce("rules.error.pending_commit", true); return false; }
    if (dirty && !window.confirm(t("rules.confirm_discard"))) return false;
    original = copy(rule);
    draftGeneration = state.generation;
    draftRules = copy(state.rules);
    selectedID = state.rules.some((item) => item.id === rule.id) ? rule.id : null;
    dirty = false;
    byID("editor-placeholder").hidden = true;
    byID("rule-form").hidden = false;
    label(byID("editor-title"), selectedID ? "rules.edit" : "rules.new");
    byID("draft-tag").hidden = true;
    byID("rule-id").value = rule.id;
    byID("rule-id").readOnly = selectedID !== null;
    byID("rule-enabled").checked = rule.enabled;
    selectPreserving(byID("event-kind"), rule.when.kind);
    byID("event-source").value = rule.when.source || "";
    byID("event-name").value = rule.when.name || "";
    byID("event-attributes").value = rule.when.attributes ? JSON.stringify(rule.when.attributes, null, 2) : "";
    for (const [id, key] of [["usb-vendor", "vendor_id"], ["usb-product", "product_id"], ["usb-serial", "serial"], ["usb-path", "path"]]) byID(id).value = rule.when.usb?.[key] ?? "";
    selectPreserving(byID("target-id"), rule.action.target_id);
    selectPreserving(byID("group-id"), rule.action.group_id);
    byID("scope-kind").value = rule.action.group_id ? "group" : rule.action.display_ids?.length ? "displays" : "target";
    for (const option of byID("display-ids").options) option.selected = (rule.action.display_ids || []).includes(option.value);
    byID("rule-priority").value = rule.priority || 0;
    byID("rule-debounce").value = rule.debounce_ms || 0;
    byID("rule-cooldown").value = rule.cooldown_ms || 0;
    label(byID("draft-detail"), "rules.draft_review_hint");
    conditionFields();
    scopeFields();
    renderRules();
    byID("rule-id").focus();
    return true;
  }
  function objectValue(id) {
    const text = byID(id).value.trim();
    if (!text) return undefined;
    const value = JSON.parse(text);
    if (!value || Array.isArray(value) || typeof value !== "object") throw failure("rules.error.object");
    return value;
  }
  function draftDocument() {
    if (!original) throw failure("rules.error.choose");
    const rule = copy(original);
    rule.id = byID("rule-id").value.trim();
    rule.enabled = byID("rule-enabled").checked;
    rule.priority = Number(byID("rule-priority").value);
    rule.debounce_ms = Number(byID("rule-debounce").value);
    rule.cooldown_ms = Number(byID("rule-cooldown").value);
    // 不支持的旧条件保留原值，防止编辑动作或启停时静默放宽已有筛选。
    if (rule.when.usb_present === undefined) {
      rule.when.kind = byID("event-kind").value;
      for (const [id, key] of [["event-source", "source"], ["event-name", "name"]]) if (!byID(id).disabled) rule.when[key] = byID(id).value.trim();
      if (!byID("event-attributes").disabled) rule.when.attributes = objectValue("event-attributes");
      for (const [id, key] of [["usb-vendor", "vendor_id"], ["usb-product", "product_id"], ["usb-serial", "serial"], ["usb-path", "path"]]) {
        if (byID(id).disabled) continue;
        rule.when.usb ||= {};
        const value = byID(id).value.trim();
        if (!value) delete rule.when.usb[key];
        else rule.when.usb[key] = key.endsWith("_id") ? Number(value) : value;
      }
      if (rule.when.usb && Object.keys(rule.when.usb).length === 0) delete rule.when.usb;
    }
    rule.action.target_id = byID("target-id").value;
    delete rule.action.display_ids;
    delete rule.action.group_id;
    // 空集合在核心协议中表示目标全部显示器，界面选择了限定范围时必须阻止空选，
    // 否则一次未完成的选择会被无意扩大为对目标内所有显示器执行。
    if (byID("scope-kind").value === "group") {
      rule.action.group_id = byID("group-id").value;
      if (!rule.action.group_id) throw failure("rules.error.choose_group");
    }
    if (byID("scope-kind").value === "displays") {
      rule.action.display_ids = Array.from(byID("display-ids").selectedOptions, (option) => option.value);
      if (!rule.action.display_ids.length) throw failure("rules.error.choose_display");
    }
    const rules = copy(draftRules);
    const index = rules.findIndex((item) => item.id === selectedID);
    if (index < 0) rules.push(rule); else rules[index] = rule;
    return {version: 1, rules};
  }
  function renderRecords() {
    byID("record-list").replaceChildren();
    byID("empty-records").hidden = (state.records || []).length > 0;
    raw(byID("empty-records"), "");
    byID("empty-records").append(label(document.createElement("span"), state.records_error_key || "rules.records.empty"));
    if (state.records_error) byID("empty-records").append(label(document.createElement("span"), "rules.original_detail", {detail: state.records_error}));
    for (const record of (state.records || []).slice().reverse()) {
      const row = document.createElement("li");
      row.className = "rules-record";
      const body = document.createElement("div");
      const title = document.createElement("strong");
      label(title, "rules.decision_title", () => ({id: record.decision.rule_id || t("rules.decision"), reason: named("rules.reason", record.decision.reason)}));
      const detail = document.createElement("p");
      if (record.error) errorDetail(detail, record.error);
      else label(detail, "rules.record_detail", () => ({event: named("rules.event", record.event?.kind || record.trigger), displays: record.decision.display_ids?.join(", ") || t("rules.no_display_action")}));
      for (const display of record.result?.displays || []) {
        const evidence = document.createElement("p");
        evidence.append(label(document.createElement("span"), "rules.display_evidence", () => ({id: display.display_id, state: named("app.result", display.state)})));
        if (display.error) errorDetail(evidence, display.error);
        body.append(evidence);
      }
      const time = document.createElement("time");
      time.dateTime = record.at;
      time.textContent = new Date(record.at).toLocaleString(window.desktopI18n.snapshot().locale, {hour12: false});
      body.prepend(title, detail);
      row.append(body, time);
      byID("record-list").append(row);
    }
  }

  byID("event-kind").addEventListener("change", () => {
    if (original && original.when.usb_present === undefined) {
      original.when = {kind: byID("event-kind").value};
      for (const id of ["event-source", "event-name", "event-attributes", "usb-vendor", "usb-product", "usb-serial", "usb-path"]) byID(id).value = "";
      announce("rules.notice.event_changed");
    }
    conditionFields();
  });
  byID("scope-kind").addEventListener("change", scopeFields);
  byID("rule-form").addEventListener("input", markDirty);
  byID("rule-form").addEventListener("change", markDirty);
  byID("new-rule").addEventListener("click", () => openEditor({id: "", enabled: false, when: {}, action: {id: "", target_id: ""}}));
  byID("cancel-draft").addEventListener("click", () => {
    if (busy || commit) return;
    original = null;
    selectedID = null;
    dirty = false;
    byID("rule-form").hidden = true;
    byID("editor-placeholder").hidden = false;
    label(byID("editor-title"), "rules.select_hint");
    byID("draft-tag").hidden = true;
    if (state) renderRules();
    announce("rules.notice.cancelled");
  });

  async function request(path, body) {
    const response = await fetch(`/rules/${path}`, {method: body === undefined ? "GET" : "POST", cache: "no-store", headers: {"Content-Type": "application/json"}, ...(body === undefined ? {} : {body: JSON.stringify(body)})});
    const value = await response.json();
    if (!response.ok) throw Object.assign(new Error(value.message || ""), value);
    return value;
  }
  function setBusy(value) {
    busy = value;
    byID("rule-form").inert = value || !!commit;
    byID("preview-form").inert = value || !!commit;
    byID("new-rule").disabled = value || !!commit || !state?.writable;
    byID("preview-rule").disabled = value || !!commit || !state?.engine_available;
    byID("refresh").disabled = value;
    byID("apply-save").disabled = value;
    byID("query-save").disabled = value;
    byID("dismiss-save").disabled = value;
    if (state) {
      byID("prepare-save").disabled = !state.writable;
      byID("validate-draft").disabled = !state.writable;
    }
  }
  async function loadState() {
    if (busy) return;
    if (original || commit) { announce("rules.error.refresh_draft", true); return; }
    setBusy(true);
    try {
      state = await request("state");
      byID("connection").dataset.connected = "true";
      label(byID("connection-title"), state.engine_available ? "rules.connected" : "rules.connected_stopped");
      label(byID("connection-detail"), state.writable ? "rules.writable_hint" : "rules.readonly_hint");
      renderCapabilities();
      options(byID("target-id"), state.targets, "rules.choose_target");
      options(byID("group-id"), state.groups, "rules.choose_group");
      options(byID("display-ids"), state.displays, "rules.choose_display");
      byID("display-ids").options[0].remove();
      renderRules();
      renderRecords();
      announce(state.records_error_key ? {summary_key: state.records_error_key, message: state.records_error} : "rules.notice.loaded");
    } catch (error) {
      state = null;
      byID("connection").dataset.connected = "false";
      label(byID("connection-title"), "rules.disconnected");
      label(byID("connection-detail"), "rules.reconnect_hint");
      announce(error, true);
    } finally { setBusy(false); }
  }
  function rememberCommit() {
    // 只保留提交标识和状态证据，不保存规则草稿。刷新或断连后仍查询同一ID，避免重复提交。
    try {
      if (commit) sessionStorage.setItem("displaykvm-rules-commit", JSON.stringify(commit));
      else sessionStorage.removeItem("displaykvm-rules-commit");
    } catch {
      // 存储失败不能丢弃当前内存中的提交，也不能以新ID重试；明确说明本页仍需保留。
      if (commit) announce("rules.error.remember_commit", true);
    }
  }
  function renderCommit() {
    byID("commit-panel").hidden = !commit;
    if (!commit) return;
    const settled = commit.phase === "failed" || (commit.phase === "applied" && commit.saved === true && commit.effective === true);
    label(byID("commit-label"), Object.prototype.hasOwnProperty.call(window.desktopI18n.snapshot().messages, `rules.phase.${commit.phase}`) ? `rules.phase.${commit.phase}` : "rules.phase.pending");
    raw(byID("commit-detail"), "");
    byID("commit-detail").append(label(document.createElement("span"), "rules.commit_evidence", () => ({
      saved: t(commit.saved === true ? "rules.saved.yes" : commit.saved === false ? "rules.saved.no" : "rules.saved.pending"),
      effective: t(commit.effective === true ? "rules.effective.yes" : commit.effective === false ? "rules.effective.no" : "rules.effective.pending")
    })));
    byID("commit-detail").append(label(document.createElement("span"), commit.generation_changed ? "rules.commit_generation_changed" : commit.phase === "accepted" ? "rules.commit_accepted" : settled ? "rules.commit_settled" : "rules.commit_query_hint"));
    if (commit.error) errorDetail(byID("commit-detail"), commit.error);
    label(byID("commit-id"), "rules.commit_id", {id: commit.id});
    byID("apply-save").hidden = commit.phase !== "accepted" || !!commit.generation_changed;
    byID("query-save").hidden = settled;
    byID("dismiss-save").hidden = !(settled || commit.phase === "accepted");
    label(byID("dismiss-save"), commit.phase === "accepted" ? "rules.dismiss_save" : "rules.close_refresh");
    rememberCommit();
    setBusy(busy);
  }
  async function validateDraft() {
    if (busy || commit) return;
    try {
      const document = draftDocument();
      setBusy(true);
      await request("validate", {generation: draftGeneration, document});
      announce("rules.notice.valid");
    } catch (error) { announce(error, true); }
    finally { setBusy(false); }
  }
  byID("validate-draft").addEventListener("click", validateDraft);
  byID("rule-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (busy || commit) return;
    try {
      const document = draftDocument();
      setBusy(true);
      await request("validate", {generation: draftGeneration, document});
      const generated = await request("commit-id", {generation: draftGeneration});
      commit = {id: generated.commit_id, prepared_generation: draftGeneration, phase: "unknown", saved: null, effective: null};
      rememberCommit();
      const response = await request("prepare", {generation: draftGeneration, commit_id: commit.id, document});
      commit = {...response.commit, generation_changed: response.generation !== response.commit.prepared_generation};
      announce("rules.notice.accepted");
    } catch (error) {
      if (commit) commit = {...commit, phase: "unknown", error: {message: error.message, code: error.code, summary_key: error.summary_key, summary_values: error.summary_values}};
      announce(error, true);
    } finally { renderCommit(); setBusy(false); }
  });
  byID("apply-save").addEventListener("click", async () => {
    if (busy || commit?.phase !== "accepted" || commit.generation_changed) return;
    setBusy(true);
    try {
      const response = await request("apply", {generation: commit.prepared_generation, commit_id: commit.id, confirmed: true});
      commit = {...response.commit, generation_changed: response.generation !== response.commit.prepared_generation && response.commit.phase === "accepted"};
      announce("rules.notice.applied");
    } catch (error) {
      commit = {...commit, phase: "unknown", saved: null, effective: null, error: {message: error.message, code: error.code, summary_key: error.summary_key, summary_values: error.summary_values}};
      announce("rules.error.apply_unknown", true);
    } finally { renderCommit(); setBusy(false); }
  });
  byID("query-save").addEventListener("click", async () => {
    if (busy || !commit) return;
    setBusy(true);
    try {
      const response = await request("commit", {commit_id: commit.id});
      commit = {...response.commit, generation_changed: response.generation !== response.commit.prepared_generation && response.commit.phase === "accepted"};
      announce("rules.notice.queried");
    } catch (error) { announce(error, true); byID("error").append(label(document.createElement("span"), "rules.error.query_reconnect")); }
    finally { renderCommit(); setBusy(false); }
  });
  byID("dismiss-save").addEventListener("click", () => {
    if (busy || !commit || !["accepted", "applied", "failed"].includes(commit.phase)) return;
    commit = null;
    original = null;
    selectedID = null;
    dirty = false;
    byID("rule-form").hidden = true;
    byID("editor-placeholder").hidden = false;
    byID("draft-tag").hidden = true;
    renderCommit();
    rememberCommit();
    loadState();
  });
  byID("preview-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    if (busy || commit || !state?.engine_available) return;
    try {
      const time = new Date(byID("preview-time").value);
      if (!Number.isFinite(time.getTime())) throw failure("rules.error.observed_at");
      const simulated = {id: `check-${crypto.randomUUID()}`, kind: byID("preview-kind").value, source: byID("preview-source").value.trim(), observed_at: time.toISOString(), name: byID("preview-name").value.trim(), usb: objectValue("preview-usb"), attributes: objectValue("preview-attributes")};
      setBusy(true);
      const response = await request("preview", {generation: state.generation, event: simulated});
      raw(byID("preview-results"), "");
      for (const decision of response.decisions || []) {
        const row = document.createElement("div");
        row.className = "rules-decision";
        row.dataset.ready = String(decision.reason === "ready");
        const title = document.createElement("strong");
        label(title, "rules.decision_title", () => ({id: decision.rule_id || t("rules.decision"), reason: named("rules.reason", decision.reason)}));
        const detail = document.createElement("p");
        label(detail, "rules.preview_detail", () => ({target: decision.request?.target_id || t("rules.none"), displays: decision.display_ids?.join(", ") || t("rules.none")}));
        row.append(title, detail);
        byID("preview-results").append(row);
      }
      if (!response.decisions?.length) label(byID("preview-results"), "rules.preview_empty");
      byID("preview-results").hidden = false;
      announce("rules.notice.previewed");
    } catch (error) { announce(error, true); }
    finally { setBusy(false); }
  });
  byID("refresh").addEventListener("click", loadState);
  byID("preview-source").value = "desktop-check";
  const now = new Date();
  const local = new Date(now.getTime() - now.getTimezoneOffset() * 60000);
  byID("preview-time").value = local.toISOString().slice(0, 19);
  try {
    const remembered = JSON.parse(sessionStorage.getItem("displaykvm-rules-commit") || "null");
    if (remembered?.id && remembered?.prepared_generation) {
      commit = {...remembered, phase: "unknown"};
      renderCommit();
      label(byID("connection-title"), "rules.pending_connection");
      label(byID("connection-detail"), "rules.pending_connection_hint");
      announce("rules.notice.remembered");
    }
  } catch { /* 无有效本页提交记录时正常加载；不读取或保存规则文件。 */ }
  // 语言更新只改已有节点文案和日期，不重建选项或编辑器；未保存的输入、选择及焦点原样保留。
  window.addEventListener("desktop:translated", () => {
    for (const node of document.querySelectorAll("[data-i18n]")) {
      const project = projections.get(node);
      if (project) project();
    }
    for (const time of document.querySelectorAll("#record-list time")) time.textContent = new Date(time.dateTime).toLocaleString(window.desktopI18n.snapshot().locale, {hour12: false});
  });
  if (!commit) loadState();
})();
