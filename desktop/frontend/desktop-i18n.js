(() => {
  "use strict";
  let catalog = {preference: "system", locale: "en", revision: 0, messages: {}};
  const text = (key, values = {}) => (catalog.messages[key] ?? key).replace(/\{([^{}]+)\}/g, (token, name) => Object.prototype.hasOwnProperty.call(values, name) ? String(values[name]) : token);
  const valuesFor = (element) => {
    try { return JSON.parse(element.dataset.i18nValues || "{}"); }
    catch (error) { console.error("Invalid translation values", error); return {}; }
  };
  const render = (root = document) => {
    for (const element of root.querySelectorAll("[data-i18n]")) {
      const translated=text(element.dataset.i18n, valuesFor(element));
      if(element.textContent!==translated) element.textContent=translated;
    }
    for (const name of ["title", "placeholder", "aria-label"]) {
      for (const element of root.querySelectorAll(`[data-i18n-${name}]`)) element.setAttribute(name, text(element.getAttribute(`data-i18n-${name}`), valuesFor(element)));
    }
    document.documentElement.lang = catalog.locale;
  };
  const apply = (next) => {
    if (!next || !next.messages || !["en", "zh-CN"].includes(next.locale) || next.revision < catalog.revision) return;
    catalog = next;
    render();
    // 只发呈现通知，不触发change/submit，不写语言草稿，不重载文档。
    window.dispatchEvent(new CustomEvent("desktop:translated", {detail: catalog}));
  };
  window.desktopI18n = {text, render, snapshot: () => catalog};
  const embedded = document.getElementById("desktop-catalog");
  if (embedded) {
    try { apply(JSON.parse(embedded.textContent)); }
    catch (error) { console.error("Invalid language catalog", error); }
  } else {
    fetch("/client/language", {cache: "no-store"}).then((response) => {
      if (!response.ok) throw new Error("Language catalog unavailable");
      return response.json();
    }).then(apply).catch((error) => console.error("Language catalog unavailable", error));
  }
  window.addEventListener("desktop:language", (event) => apply(event.detail));
  // 文件操作原位导入新表单后只翻译新增文字，不改任何输入值、选中项或焦点。
  new MutationObserver((records) => {
    for (const record of records) for (const node of record.addedNodes) {
      if (node.nodeType !== 1) continue;
      if (node.matches("[data-i18n]")) {
        const translated=text(node.dataset.i18n,valuesFor(node));
        if(node.textContent!==translated) node.textContent=translated;
      }
      render(node);
    }
  }).observe(document.body, {childList: true, subtree: true});
})();
