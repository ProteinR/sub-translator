// Работа со страницей Lokalise (Multilingual view) изнутри вкладки WebKit.
// Go вызывает window.__loka[op](args); каждая функция возвращает JSON-совместимый объект.
(function () {
  const VERSION = 2;
  if (window.__loka && window.__loka.v === VERSION) return;

  const ROW = ".row-key[data-id]";
  const EDITOR = ".ace_text-input, textarea:not([style*='display: none']), [contenteditable='true']";
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const esc = (s) => (window.CSS && CSS.escape ? CSS.escape(s) : String(s).replace(/["\\]/g, "\\$&"));
  const norm = (s) => String(s || "").replace(/\u00a0/g, " ").split(/\s+/).filter(Boolean).join(" ");

  function visible(el) {
    if (!el || !el.isConnected) return false;
    const r = el.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) return false;
    const st = getComputedStyle(el);
    return st.visibility !== "hidden" && st.display !== "none";
  }

  async function waitFor(fn, timeout, step = 50) {
    const end = Date.now() + timeout;
    for (;;) {
      const v = fn();
      if (v) return v;
      if (Date.now() > end) return null;
      await sleep(step);
    }
  }

  function cellValue(cell) {
    const vals = cell.querySelectorAll("[data-lokalise-editor-value]");
    if (vals.length === 1) return vals[0].getAttribute("data-lokalise-editor-value") || "";
    if (cell.querySelector(".highlight .empty")) return "";
    const h = cell.querySelector(".highlight");
    return h ? h.innerText : "";
  }

  function rowEl(id) {
    return document.querySelector(`.row-key[data-id="${esc(id)}"]`);
  }

  function targetCell(row, langId) {
    return row.querySelector(`.key-translation[data-lang-id="${esc(langId)}"]:not([data-is-base='1'])`);
  }

  // Прокручиваемый контейнер списка строк: окно или ближайший предок со скроллом.
  function scroller() {
    const row = document.querySelector(ROW);
    for (let el = row && row.parentElement; el && el !== document.body; el = el.parentElement) {
      const st = getComputedStyle(el);
      if (/(auto|scroll)/.test(st.overflowY) && el.scrollHeight > el.clientHeight + 10) return el;
    }
    return document.scrollingElement || document.documentElement;
  }

  // Клик «как мышью»: Lokalise слушает mousedown/mouseup, а не только click.
  function press(el) {
    const r = el.getBoundingClientRect();
    const o = { bubbles: true, cancelable: true, view: window, button: 0, clientX: r.left + Math.min(20, r.width / 2), clientY: r.top + Math.min(10, r.height / 2) };
    el.dispatchEvent(new PointerEvent("pointerdown", o));
    el.dispatchEvent(new MouseEvent("mousedown", o));
    el.dispatchEvent(new PointerEvent("pointerup", o));
    el.dispatchEvent(new MouseEvent("mouseup", o));
    el.dispatchEvent(new MouseEvent("click", o));
  }

  function findEditor(cell) {
    // Сейчас Lokalise использует CodeMirror 5; остальные варианты — на случай старого/нового редактора.
    const cm = cell.querySelector(".CodeMirror");
    if (cm && cm.CodeMirror && visible(cm)) return cm;
    for (const el of cell.querySelectorAll(EDITOR)) {
      if (el.classList.contains("ace_text-input")) {
        const ace = el.closest(".ace_editor");
        if (ace && visible(ace)) return el;
      } else if (visible(el)) {
        return el;
      }
    }
    return null;
  }

  function setEditorText(editor, text) {
    if (editor.CodeMirror) {
      const cm = editor.CodeMirror;
      cm.focus();
      cm.setValue(text);
      cm.setCursor(cm.lineCount(), 0);
      return "codemirror";
    }
    editor.focus();
    const aceEl = editor.closest(".ace_editor");
    const ace = aceEl && (aceEl.env && aceEl.env.editor);
    if (ace) {
      ace.setValue(text, 1);
      return "ace";
    }
    if (editor.tagName === "TEXTAREA" || editor.tagName === "INPUT") {
      editor.select();
      if (!document.execCommand("insertText", false, text)) {
        const proto = editor.tagName === "TEXTAREA" ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
        Object.getOwnPropertyDescriptor(proto, "value").set.call(editor, text);
        editor.dispatchEvent(new Event("input", { bubbles: true }));
      }
      return "textarea";
    }
    // contenteditable
    const sel = getSelection();
    const range = document.createRange();
    range.selectNodeContents(editor);
    sel.removeAllRanges();
    sel.addRange(range);
    if (!document.execCommand("insertText", false, text)) {
      editor.textContent = text;
      editor.dispatchEvent(new InputEvent("input", { bubbles: true, inputType: "insertText", data: text }));
    }
    return "contenteditable";
  }

  window.__loka = {
    v: VERSION,

    state() {
      const login = [...document.querySelectorAll("h1")].some((h) => /log in|sign in/i.test(h.textContent));
      return {
        url: location.href,
        ready: document.readyState,
        rows: document.querySelectorAll(ROW).length,
        login,
        files: [...document.querySelectorAll(".project-document__file-name")].map((e) => e.textContent.trim()).filter(Boolean),
      };
    },

    // Все строки, которые сейчас есть в DOM (список виртуализирован).
    scan() {
      const rows = [];
      for (const row of document.querySelectorAll(ROW)) {
        const bases = row.querySelectorAll(".key-translation[data-is-base='1']");
        const base = bases[0];
        rows.push({
          id: row.getAttribute("data-id") || "",
          file: row.getAttribute("data-filename") || "",
          baseCount: bases.length,
          sourceLang: base ? base.getAttribute("data-lang") || "" : "",
          sourceId: base ? base.getAttribute("data-lang-id") || "" : "",
          source: base ? cellValue(base) : "",
          targets: [...row.querySelectorAll(".key-translation:not([data-is-base='1'])")].map((c) => ({
            langId: c.getAttribute("data-lang-id") || "",
            lang: c.getAttribute("data-lang") || "",
            value: cellValue(c),
          })),
        });
      }
      return rows;
    },

    scroll({ dy }) {
      const el = scroller();
      const before = el.scrollTop;
      if (el === document.scrollingElement || el === document.documentElement) window.scrollBy(0, dy);
      else el.scrollTop += dy;
      el.dispatchEvent(new Event("scroll"));
      return { top: el.scrollTop, moved: el.scrollTop !== before, max: el.scrollHeight - el.clientHeight };
    },

    top() {
      const el = scroller();
      if (el === document.scrollingElement || el === document.documentElement) window.scrollTo(0, 0);
      else el.scrollTop = 0;
      return { top: el.scrollTop };
    },

    has({ id }) {
      return !!rowEl(id);
    },

    // Вставка одного перевода. status: ok | skipped | missing; при сбое бросает исключение.
    async fill({ id, langId, text, overwrite, attempt, editorDelay, saveDelay }) {
      const row = rowEl(id);
      if (!row) return { status: "missing" };
      row.scrollIntoView({ block: "center" });
      const cell = targetCell(row, langId);
      if (!cell) throw new Error(`нет ячейки языка ${langId} в строке ${id}`);
      if (attempt === 0 && !overwrite && cellValue(cell).trim() !== "") return { status: "skipped" };

      const target = cell.querySelector(".highlight") || cell;
      let editor = findEditor(cell);
      if (!editor) {
        press(target);
        editor = await waitFor(() => findEditor(cell), 5000);
      }
      if (!editor) {
        target.click();
        editor = await waitFor(() => findEditor(cell), 3000);
      }
      if (!editor) throw new Error(`редактор не открылся (строка ${id})`);
      await sleep(editorDelay || 0);
      const kind = setEditorText(editor, text);
      await sleep(saveDelay || 0);

      const save = await waitFor(() => {
        const b = cell.querySelector("button.save.btn-primary");
        return b && visible(b) && !b.disabled ? b : null;
      }, 5000);
      if (!save) throw new Error(`нет кнопки Save (строка ${id}, редактор ${kind})`);
      save.click();

      await waitFor(() => !findEditor(cell), 5000);
      const saved = await waitFor(() => (norm(cellValue(cell)) === norm(text) ? true : null), 3000, 100);
      if (!saved) throw new Error(`сохранение не подтвердилось (строка ${id}, редактор ${kind})`);
      return { status: "ok", kind };
    },

    html() {
      return "<!doctype html>\n" + document.documentElement.outerHTML;
    },

    // Закрыть зависший редактор.
    escape() {
      const el = document.activeElement || document.body;
      for (const type of ["keydown", "keyup"]) {
        el.dispatchEvent(new KeyboardEvent(type, { key: "Escape", code: "Escape", keyCode: 27, which: 27, bubbles: true }));
      }
      for (const b of document.querySelectorAll("button.cancel, .btn-cancel")) if (visible(b)) b.click();
      return true;
    },
  };
})();
