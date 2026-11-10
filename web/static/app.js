// dramadrama workbench: small progressive enhancements on top of plain
// server-rendered forms.
//  - run buttons stream step output (Server-Sent Events over fetch)
//  - asset picker dialog for attaching library assets
//  - scroll position kept across form posts / reloads
//  - unsaved edits are marked, and saved automatically before a run or
//    another form post would reload the page and lose them
//  - step navigation highlights the section in view
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
  // The page is made of many small forms that each save one thing. Edits in
  // a form are tracked ("dirty") so they are not lost when something else
  // reloads the page: dirty forms are posted in the background first.
  // Only forms marked data-save (plain "edit and save" forms) take part.
  function tracked(form) { return !!form && form.hasAttribute("data-save"); }
  document.addEventListener("input", (ev) => {
    const form = ev.target.form;
    if (tracked(form)) form.classList.add("dirty");
  });

  // saveDirty posts every dirty form except `skip`. It resolves to false
  // (after telling the user) if a save failed.
  async function saveDirty(skip) {
    for (const form of document.querySelectorAll("form.dirty")) {
      if (form === skip) continue;
      try {
        // getAttribute: a field named "action" would shadow form.action.
        const resp = await fetch(form.getAttribute("action"), { method: "POST", body: new URLSearchParams(new FormData(form)) });
        const err = new URL(resp.url).searchParams.get("err");
        if (!resp.ok || err) throw new Error(err || resp.statusText);
        form.classList.remove("dirty");
      } catch (err) {
        form.scrollIntoView({ block: "center" });
        alert(T("Could not save your edits: ") + err.message);
        return false;
      }
    }
    return true;
  }

  // submitForm saves other dirty forms, then submits `form` natively.
  async function submitForm(form) {
    if (!(await saveDirty(form))) return;
    form.classList.remove("dirty");
    saveScroll();
    form.submit();
  }

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
    if ((form.getAttribute("method") || "").toLowerCase() !== "post") return;
    if (ev.submitter && ev.submitter.hasAttribute("formaction")) { // e.g. "reset to default"
      form.classList.remove("dirty");
      saveScroll();
      return;
    }
    ev.preventDefault();
    if (form.dataset.busy && ev.submitter) { // slow posts: show progress on the button
      ev.submitter.disabled = true;
      ev.submitter.textContent = form.dataset.busy;
    }
    submitForm(form);
  });

  document.addEventListener("change", (ev) => {
    const el = ev.target;
    if (el.matches("input[type=file][data-autosubmit]") && el.files.length) submitForm(el.form);
  });

  window.addEventListener("beforeunload", (ev) => {
    if (document.querySelector("form.dirty")) ev.preventDefault();
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
    if (!(await saveDirty())) return; // the run reads saved data, and reloads the page
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

  // ---------------------------------------------------------------- dialogs
  document.addEventListener("click", (ev) => {
    const opener = ev.target.closest("[data-dialog]");
    if (opener) {
      const dlg = document.getElementById(opener.dataset.dialog);
      if (dlg) dlg.showModal();
      return;
    }
    const closer = ev.target.closest("[data-close]");
    if (closer) closer.closest("dialog").close();
    else if (ev.target.matches("dialog.modal")) ev.target.close(); // click on the backdrop
  });

  // ---------------------------------------------------------------- model picker
  const mp = document.getElementById("model-picker");
  if (mp) {
    const list = document.getElementById("mp-list");
    const cat = document.getElementById("mp-cat");
    const q = document.getElementById("mp-q");
    const info = document.getElementById("mp-info");
    const refresh = document.getElementById("mp-refresh");
    const MAX = 300;
    let models = [];
    let provider = "";
    let input = null;

    function render() {
      const words = q.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
      const hits = models.filter((m) => {
        if (cat.value && m.category !== cat.value) return false;
        const hay = (m.id + " " + (m.name || "") + " " + (m.description || "")).toLowerCase();
        return words.every((w) => hay.includes(w));
      });
      list.textContent = "";
      if (!models.length) { list.innerHTML = "<p></p>"; list.firstChild.textContent = T("No model list yet. Fetch it from the provider first."); return; }
      if (!hits.length) { list.innerHTML = "<p></p>"; list.firstChild.textContent = T("No models match."); return; }
      for (const m of hits.slice(0, MAX)) {
        const b = document.createElement("button");
        b.type = "button";
        b.className = "model-row" + (input && input.value === m.id ? " current" : "");
        const name = document.createElement("span"); name.className = "m-name"; name.textContent = m.name || m.id;
        const id = document.createElement("span"); id.className = "m-id"; id.textContent = m.id;
        b.append(name, id);
        if (m.category) { const t = document.createElement("span"); t.className = "tag"; t.textContent = m.category; b.append(t); }
        if (m.description) { const d = document.createElement("span"); d.className = "m-desc"; d.textContent = m.description; b.append(d); }
        b.addEventListener("click", () => {
          input.value = m.id;
          input.dispatchEvent(new Event("input", { bubbles: true })); // marks the form dirty
          mp.close();
          input.focus();
        });
        list.appendChild(b);
      }
      if (hits.length > MAX) {
        const p = document.createElement("p");
        p.textContent = T("Showing the first 300; type to narrow down.");
        list.appendChild(p);
      }
    }

    async function load(wantCategory) {
      list.innerHTML = "<p></p>"; list.firstChild.textContent = T("Loading…");
      info.textContent = "";
      refresh.textContent = T("Fetch model list");
      try {
        const resp = await fetch("/api/models?provider=" + encodeURIComponent(provider));
        if (!resp.ok) throw new Error((await resp.text()) || resp.statusText);
        const data = await resp.json();
        models = data.models || [];
        const cats = [...new Set(models.map((m) => m.category).filter(Boolean))].sort();
        cat.textContent = "";
        cat.hidden = cats.length < 2;
        cat.add(new Option(T("All categories"), ""));
        for (const c of cats) cat.add(new Option(c, c));
        cat.value = cats.includes(wantCategory) ? wantCategory : "";
        if (models.length) {
          info.textContent = models.length + " " + T("models") + " · " + T("Updated") + " " + data.fetched_at;
          refresh.textContent = T("Refresh");
        }
        render();
      } catch (err) {
        models = [];
        list.firstChild.textContent = T("Failed to load: ") + err.message;
      }
    }

    refresh.addEventListener("click", async () => {
      refresh.disabled = true;
      refresh.textContent = T("Fetching…");
      try {
        const resp = await fetch("/settings/providers/models", {
          method: "POST", headers: { Accept: "application/json" }, body: new URLSearchParams({ name: provider }),
        });
        const data = await resp.json();
        if (!resp.ok) throw new Error(data.error || resp.statusText);
        await load(cat.value);
      } catch (err) {
        info.textContent = T("Failed: ") + err.message;
        refresh.textContent = T("Fetch model list");
      } finally {
        refresh.disabled = false;
      }
    });

    document.addEventListener("click", (ev) => {
      const btn = ev.target.closest("[data-pick-model]");
      if (!btn) return;
      const scope = btn.closest("[data-model-scope]");
      const sel = scope && scope.querySelector("select[data-provider]");
      input = btn.parentElement.querySelector("input");
      provider = sel ? sel.value : "";
      if (!provider) { alert(T("Select a provider first.")); return; }
      document.getElementById("mp-provider").textContent = provider;
      q.value = "";
      mp.showModal();
      load(btn.dataset.category || "");
      q.focus();
    });
    cat.addEventListener("change", render);
    q.addEventListener("input", render);
  }

  // ---------------------------------------------------------------- step nav
  const steps = [...document.querySelectorAll(".stepper .step")];
  if (steps.length && "IntersectionObserver" in window) {
    const visible = new Set();
    const mark = () => {
      const first = steps.find((a) => visible.has(a.hash.slice(1))) || null;
      steps.forEach((a) => a.classList.toggle("current", a === first));
    };
    const io = new IntersectionObserver((entries) => {
      for (const e of entries) e.isIntersecting ? visible.add(e.target.id) : visible.delete(e.target.id);
      mark();
    }, { rootMargin: "-25% 0px -55% 0px" });
    steps.forEach((a) => { const sec = document.getElementById(a.hash.slice(1)); if (sec) io.observe(sec); });
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
      picker.close();
      submitForm(form);
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
