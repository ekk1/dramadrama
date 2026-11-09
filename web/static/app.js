// dramadrama workbench: small progressive enhancements on top of plain
// server-rendered forms.
//  - run buttons stream step output (Server-Sent Events over fetch)
//  - asset picker dialog for attaching library assets
//  - scroll position kept across form posts / reloads
//  - confirm prompts, auto-submit uploads, dismissible flash messages
"use strict";

(function () {
  // Translated UI strings injected by the server (window.I18N); fall back to English.
  const T = (k) => (window.I18N && window.I18N[k]) || k;

  const store = {
    get(k) { try { return sessionStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { sessionStorage.setItem(k, v); } catch { /* ignore */ } },
    del(k) { try { sessionStorage.removeItem(k); } catch { /* ignore */ } },
  };

  // ---------------------------------------------------------------- scroll
  const scrollKey = "scroll:" + location.pathname;
  function saveScroll() { store.set(scrollKey, String(window.scrollY)); }
  window.addEventListener("load", () => {
    const y = store.get(scrollKey);
    if (y !== null && !location.hash) window.scrollTo(0, Number(y));
    store.del(scrollKey);
  });

  // ---------------------------------------------------------------- forms
  document.addEventListener("submit", (ev) => {
    const form = ev.target;
    if (form.dataset.confirm && !confirm(form.dataset.confirm)) {
      ev.preventDefault();
      return;
    }
    if (form.hasAttribute("data-run")) {
      ev.preventDefault();
      runStep(form);
      return;
    }
    if ((form.method || "").toLowerCase() === "post") saveScroll();
  });

  document.addEventListener("change", (ev) => {
    const el = ev.target;
    if (el.matches("input[type=file][data-autosubmit]") && el.files.length) {
      saveScroll();
      el.form.submit();
    }
  });

  document.addEventListener("click", (ev) => {
    const close = ev.target.closest(".flash-close");
    if (close) close.parentElement.remove();
  });

  // ---------------------------------------------------------------- runs
  function logParts(id) {
    const box = document.getElementById(id);
    if (!box) return null;
    return {
      box,
      pre: box.querySelector("pre"),
      state: box.querySelector(".run-log-state"),
      stop: box.querySelector(".run-log-stop"),
      close: box.querySelector(".run-log-close"),
    };
  }

  function append(pre, text, cls) {
    if (cls) {
      const span = document.createElement("span");
      span.className = cls;
      span.textContent = text;
      pre.appendChild(span);
    } else {
      pre.appendChild(document.createTextNode(text));
    }
    pre.scrollTop = pre.scrollHeight;
  }

  // Persist the log across the reload that follows a run.
  function persistLog(id, log, stateText, failed) {
    store.set("runlog:" + location.pathname + ":" + id, JSON.stringify({ html: log.pre.innerHTML, state: stateText, failed }));
  }

  function restoreLogs() {
    document.querySelectorAll(".run-log").forEach((box) => {
      const key = "runlog:" + location.pathname + ":" + box.id;
      const raw = store.get(key);
      if (!raw) return;
      store.del(key);
      try {
        const saved = JSON.parse(raw);
        const log = logParts(box.id);
        log.pre.innerHTML = saved.html;
        log.state.textContent = saved.state;
        box.hidden = false;
        if (saved.failed) box.scrollIntoView({ block: "center" });
      } catch { /* ignore */ }
    });
  }
  restoreLogs();

  document.addEventListener("click", (ev) => {
    const btn = ev.target.closest(".run-log-close");
    if (btn) btn.closest(".run-log").hidden = true;
  });

  let activeRuns = 0;
  window.addEventListener("beforeunload", (ev) => {
    if (activeRuns > 0) {
      // The step keeps running on the server, but warn anyway.
      ev.preventDefault();
    }
  });

  async function runStep(form) {
    const fd = new FormData(form);
    const optsId = form.dataset.opts;
    if (optsId) {
      const opts = document.getElementById(optsId);
      if (opts) {
        opts.querySelectorAll("input, select, textarea").forEach((el) => {
          if (!el.name || fd.has(el.name)) return;
          if (el.type === "checkbox" && !el.checked) return;
          fd.set(el.name, el.value);
        });
      }
    }
    const logId = form.dataset.log;
    const log = logParts(logId);
    const button = form.querySelector("button");
    const label = button.textContent;
    button.disabled = true;
    button.textContent = T("Running…");
    if (log) {
      log.box.hidden = false;
      log.box.classList.add("running");
      log.pre.textContent = "";
      log.state.textContent = label + "…";
      log.box.scrollIntoView({ block: "nearest" });
    }

    let runKey = "";
    let failed = false;
    let stateText = "";
    activeRuns++;
    const onStop = async () => {
      if (!runKey) return;
      const body = new URLSearchParams({ key: runKey });
      await fetch(form.action.replace(/\/run\/[^/]+$/, "/cancel"), { method: "POST", body, redirect: "manual" });
    };
    if (log) {
      log.stop.hidden = false;
      log.stop.onclick = onStop;
    }

    try {
      const resp = await fetch(form.action, { method: "POST", body: new URLSearchParams(fd) });
      if (!resp.ok || !resp.body) throw new Error((await resp.text()) || resp.statusText);
      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buf = "";
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          const chunk = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          let event = "message";
          const data = [];
          for (const line of chunk.split("\n")) {
            if (line.startsWith("event:")) event = line.slice(6).trim();
            else if (line.startsWith("data:")) data.push(line.slice(5).replace(/^ /, ""));
          }
          let text = "";
          try { text = JSON.parse(data.join("\n")); } catch { text = data.join("\n"); }
          if (!log) continue;
          switch (event) {
            case "key": runKey = text; break;
            case "delta": append(log.pre, text); break;
            case "status": append(log.pre, "\n› " + text + "\n", "st"); break;
            case "error":
              failed = true;
              stateText = T("Failed: ") + text;
              append(log.pre, "\n✗ " + text + "\n", "er");
              break;
            case "done": stateText = label + ": " + T("done"); break;
          }
        }
      }
    } catch (err) {
      failed = true;
      stateText = T("Failed: ") + err.message;
      if (log) append(log.pre, "\n✗ " + err.message + "\n", "er");
    } finally {
      activeRuns--;
    }
    if (log) persistLog(logId, log, stateText || (label + ": " + T("finished")), failed);
    saveScroll();
    location.reload();
  }

  // ---------------------------------------------------------------- picker
  const picker = document.getElementById("picker");
  if (picker) {
    const grid = document.getElementById("picker-grid");
    const scope = document.getElementById("picker-scope");
    const kind = document.getElementById("picker-kind");
    const q = document.getElementById("picker-q");
    let ctx = null;
    let timer = null;
    const projectID = (location.pathname.match(/^\/projects\/([^/]+)/) || [])[1] || "";

    async function load() {
      const params = new URLSearchParams();
      if (scope.value === "project" && projectID) params.set("project", projectID);
      if (kind.value) params.set("kind", kind.value);
      if (q.value.trim()) params.set("q", q.value.trim());
      grid.textContent = T("Loading…");
      try {
        const resp = await fetch("/api/assets?" + params);
        const items = await resp.json();
        grid.textContent = "";
        if (!items.length) { grid.textContent = T("No assets found."); return; }
        for (const it of items) {
          const b = document.createElement("button");
          b.type = "button";
          b.title = [it.name, it.kind, it.project].filter(Boolean).join(" · ");
          const media = document.createElement(it.mime.startsWith("video/") ? "video" : "img");
          media.src = it.url;
          if (media.tagName === "IMG") { media.loading = "lazy"; media.alt = it.name; }
          const cap = document.createElement("span");
          cap.textContent = (it.name || it.id) + " · " + it.kind;
          b.append(media, cap);
          b.addEventListener("click", () => choose(it.id));
          grid.appendChild(b);
        }
      } catch (err) {
        grid.textContent = T("Failed to load: ") + err.message;
      }
    }

    function choose(id) {
      const form = document.createElement("form");
      form.method = "post";
      form.action = ctx.action;
      const fields = { target: ctx.target, asset_id: id, op: "add", back: ctx.back || location.pathname };
      for (const [k, v] of Object.entries(fields)) {
        const input = document.createElement("input");
        input.type = "hidden"; input.name = k; input.value = v;
        form.appendChild(input);
      }
      document.body.appendChild(form);
      saveScroll();
      form.submit();
    }

    document.addEventListener("click", (ev) => {
      const btn = ev.target.closest("[data-pick]");
      if (!btn) return;
      ctx = { action: btn.dataset.action, target: btn.dataset.target, back: btn.dataset.back };
      scope.value = projectID ? "project" : "";
      picker.showModal();
      load();
    });
    scope.addEventListener("change", load);
    kind.addEventListener("change", load);
    q.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(load, 250); });
  }
})();
