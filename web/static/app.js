// dramadrama workbench: small progressive enhancements on top of plain
// server-rendered forms.
//  - run buttons stream step output (Server-Sent Events over fetch)
//  - asset picker dialog for attaching library assets
//  - on live pages (the project page) form posts and finished runs update
//    the page in place instead of reloading it; elsewhere the scroll
//    position is kept across the reload
//  - unsaved edits are marked, and saved automatically before a run or
//    another form post would reload the page and lose them
//  - a page picks up the runs it finds in progress when it loads
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

  // ---------------------------------------------------------------- live pages
  // A page whose content carries [data-live] is updated in place: a form
  // post is sent with fetch, and the page the server answers with is merged
  // into the current one. Only what changed is touched, so the scroll
  // position, playing videos, open run logs and unsaved edits all survive.
  const main = document.querySelector("main");
  const live = !!(main && main.querySelector("[data-live]"));

  // Parts of the page that belong to the user or to a script, not to the
  // server: forms with unsaved edits, run logs, and running run buttons.
  function keep(el) { return el.matches("form.dirty, .run-log, form.running"); }

  const ATOMIC = new Set(["INPUT", "TEXTAREA", "SELECT", "OPTION", "VIDEO", "AUDIO"]);

  function syncNode(old, fresh) {
    if (old.nodeType !== 1) {
      if (old.nodeValue !== fresh.nodeValue) old.nodeValue = fresh.nodeValue;
      return;
    }
    if (keep(old) || old.isEqualNode(fresh)) return;
    if (ATOMIC.has(old.tagName)) { // a new element also takes the new value
      old.replaceWith(document.importNode(fresh, true));
      return;
    }
    for (const a of [...old.attributes]) if (!fresh.hasAttribute(a.name)) old.removeAttribute(a.name);
    for (const a of fresh.attributes) if (old.getAttribute(a.name) !== a.value) old.setAttribute(a.name, a.value);
    syncChildren(old, fresh);
  }

  // Two nodes are the same item if they have the same id; nodes without
  // one are paired by position when they are the same kind of node.
  function samePlace(old, fresh) {
    if (old.nodeType !== fresh.nodeType) return false;
    if (old.nodeType !== 1) return true;
    if (old.id || fresh.id) return false; // ids are paired by lookup
    if (old.tagName !== fresh.tagName) return false;
    return old.tagName !== "FORM" || old.getAttribute("action") === fresh.getAttribute("action");
  }

  function syncChildren(old, fresh) {
    const byId = new Map();
    for (const c of old.children) if (c.id) byId.set(c.id, c);
    let cur = old.firstChild;
    for (const n of [...fresh.childNodes]) {
      let match = null;
      if (n.nodeType === 1 && n.id) match = byId.get(n.id) || null;
      else if (cur && samePlace(cur, n)) match = cur;
      if (!match) {
        old.insertBefore(document.importNode(n, true), cur);
        continue;
      }
      if (match === cur) cur = cur.nextSibling;
      else old.insertBefore(match, cur);
      syncNode(match, n);
    }
    while (cur) {
      const next = cur.nextSibling;
      cur.remove();
      cur = next;
    }
  }

  // apply merges a page fetched from the server into the current one.
  function apply(html) {
    const doc = new DOMParser().parseFromString(html, "text/html");
    const fresh = doc.querySelector("main");
    if (!fresh) return;
    document.querySelectorAll("body > .flash").forEach((el) => el.remove());
    doc.querySelectorAll("body > .flash").forEach((el) => main.before(document.importNode(el, true)));
    syncChildren(main, fresh);
    if (doc.title) document.title = doc.title;
  }

  function flashError(text) {
    document.querySelectorAll("body > .flash").forEach((el) => el.remove());
    const div = document.createElement("div");
    div.className = "flash error";
    div.setAttribute("role", "alert");
    div.textContent = text;
    const close = document.createElement("button");
    close.type = "button";
    close.className = "flash-close";
    close.textContent = "×";
    div.appendChild(close);
    main.before(div);
  }

  // Posts and refreshes run one after another, so the page always ends up
  // showing the newest state.
  let queue = Promise.resolve();
  function queued(fn) {
    const p = queue.then(fn);
    queue = p.catch(() => {});
    return p;
  }

  // refresh re-reads the current page and merges it in.
  function refresh() {
    return queued(async () => {
      try {
        const resp = await fetch(location.pathname + location.search, { headers: { Accept: "text/html" } });
        if (resp.ok) apply(await resp.text());
      } catch { /* the next refresh will catch up */ }
    });
  }

  let refreshTimer = null;
  function refreshSoon() {
    if (!live || refreshTimer) return;
    refreshTimer = setTimeout(() => { refreshTimer = null; refresh(); }, 1500);
  }

  // postLive sends a form in the background and merges the answer. It
  // resolves to false if the post failed.
  function postLive(form) {
    return queued(async () => {
      const multipart = (form.getAttribute("enctype") || "").startsWith("multipart/");
      const fd = new FormData(form);
      fd.set("back", location.pathname + location.search); // answer with this page
      let resp;
      try {
        // getAttribute: a field named "action" would shadow form.action.
        resp = await fetch(form.getAttribute("action"), { method: "POST", body: multipart ? fd : new URLSearchParams(fd) });
      } catch (err) {
        flashError(T("Failed: ") + err.message);
        return false;
      }
      const url = new URL(resp.url);
      if (!resp.ok) {
        flashError(T("Failed: ") + ((await resp.text()).trim() || resp.statusText));
        return false;
      }
      if (url.pathname !== location.pathname) { // e.g. the project was deleted
        location.href = url.href;
        return true;
      }
      const failed = url.searchParams.has("err");
      if (!failed) form.classList.remove("dirty");
      apply(await resp.text());
      return !failed;
    });
  }

  // submitForm saves other dirty forms, then submits `form`: in place on a
  // live page, natively (with a reload) elsewhere.
  async function submitForm(form, submitter) {
    if (!(await saveDirty(form))) return;
    if (!live) {
      form.classList.remove("dirty");
      saveScroll();
      form.submit();
      return;
    }
    const label = submitter ? submitter.textContent : "";
    if (submitter) submitter.disabled = true;
    await postLive(form);
    if (submitter && submitter.isConnected) {
      submitter.disabled = false;
      submitter.textContent = label;
    }
    if (form.querySelector("input[type=file]")) form.reset();
  }

  document.addEventListener("submit", (ev) => {
    const form = ev.target;
    if (form.dataset.confirm && !confirm(form.dataset.confirm)) {
      ev.preventDefault();
      return;
    }
    if (form.hasAttribute("data-run")) {
      ev.preventDefault();
      startRun(form);
      return;
    }
    if ((form.getAttribute("method") || "").toLowerCase() !== "post") return;
    if (ev.submitter && ev.submitter.hasAttribute("formaction")) { // e.g. "reset to default"
      form.classList.remove("dirty");
      saveScroll();
      return;
    }
    ev.preventDefault();
    const label = ev.submitter ? ev.submitter.textContent : "";
    if (form.dataset.busy && ev.submitter) { // slow posts: show progress on the button
      ev.submitter.disabled = true;
      ev.submitter.textContent = form.dataset.busy;
    }
    submitForm(form, ev.submitter).then(() => {
      if (live && ev.submitter) ev.submitter.textContent = label;
    });
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

  document.addEventListener("click", (ev) => {
    const btn = ev.target.closest(".run-log-close");
    if (btn) btn.closest(".run-log").hidden = true;
  });

  // startRun posts a run form; the answer is the run's event stream.
  async function startRun(form) {
    if (!(await saveDirty())) return; // the run reads saved data
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
    await followRun(form, () => fetch(form.getAttribute("action"), { method: "POST", body: new URLSearchParams(fd) }));
  }

  // The runs this page found in progress when it loaded keep going on the
  // server; follow them from their first event.
  function resumeRuns() {
    const base = (location.pathname.match(/^\/projects\/[^/]+/) || [])[0];
    if (!base) return;
    const keys = new Set([...document.querySelectorAll("[data-running]")].map((el) => el.dataset.running));
    document.querySelectorAll("form[data-run]").forEach((form) => {
      if (!keys.has(form.dataset.key)) return;
      followRun(form, () => fetch(base + "/events?key=" + encodeURIComponent(form.dataset.key)), true);
    });
  }

  // followRun shows a run's event stream in the form's log until it ends.
  // With `resumed` a missing stream is not an error: the run has just ended.
  async function followRun(form, open, resumed) {
    if (form.classList.contains("running")) return;
    const logId = form.dataset.log;
    const log = logParts(logId);
    const button = form.querySelector("button");
    const label = button.textContent;
    button.disabled = true;
    button.textContent = T("Running…");
    form.classList.add("running");
    if (log) {
      log.box.hidden = false;
      log.box.classList.remove("failed");
      log.box.classList.add("running");
      log.pre.textContent = "";
      log.state.textContent = label + "…";
      if (!resumed) log.box.scrollIntoView({ block: "nearest" });
    }

    let runKey = form.dataset.key || "";
    let failed = false;
    let stateText = "";
    const onStop = async () => {
      if (!runKey) return;
      const body = new URLSearchParams({ key: runKey });
      await fetch(form.getAttribute("action").replace(/\/run\/[^/]+$/, "/cancel"), { method: "POST", body, redirect: "manual" });
    };
    if (log) {
      log.stop.hidden = false;
      log.stop.onclick = onStop;
    }

    try {
      const resp = await open();
      if (resumed && resp.status === 404) {
        if (log) log.box.hidden = true;
      } else {
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
            if (event === "key") { runKey = text; refreshSoon(); } // the page now shows it as running
            if (!log) continue;
            switch (event) {
              case "delta": append(log.pre, text); break;
              case "status":
                append(log.pre, "\n› " + text + "\n", "st");
                refreshSoon(); // show results of a long run as they arrive
                break;
              case "error":
                failed = true;
                stateText = T("Failed: ") + text;
                append(log.pre, "\n✗ " + text + "\n", "er");
                break;
              case "done": stateText = label + ": " + T("done"); break;
            }
          }
        }
      }
    } catch (err) {
      failed = true;
      stateText = T("Failed: ") + err.message;
      if (log) append(log.pre, "\n✗ " + err.message + "\n", "er");
    }
    form.classList.remove("running");
    button.disabled = false;
    button.textContent = label;
    if (log) {
      log.box.classList.remove("running");
      log.box.classList.toggle("failed", failed);
      log.stop.hidden = true;
      log.state.textContent = stateText || (label + ": " + T("finished"));
    }
    if (live) {
      await refresh();
    } else {
      saveScroll();
      location.reload();
    }
  }
  resumeRuns();

  // Step links leave the page: save unsaved edits first rather than ask.
  document.addEventListener("click", (ev) => {
    const link = ev.target.closest(".stepper a, .step-pager a");
    if (!link || !document.querySelector("form.dirty")) return;
    ev.preventDefault();
    saveDirty().then((ok) => { if (ok) location.href = link.href; });
  });

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
      if (ctx && ctx.media) params.set("media", ctx.media);
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
          let media;
          if (it.mime.startsWith("audio/") || it.mime === "application/ogg") {
            media = document.createElement("div");
            media.className = "audio-pick";
            media.textContent = "♪";
          } else {
            media = document.createElement(it.mime.startsWith("video/") ? "video" : "img");
            media.src = it.url;
            if (media.tagName === "IMG") { media.loading = "lazy"; media.alt = it.name; }
            else { media.muted = true; media.preload = "metadata"; }
          }
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
      submitForm(form).then(() => { if (live) form.remove(); });
    }

    document.addEventListener("click", (ev) => {
      const btn = ev.target.closest("[data-pick]");
      if (!btn) return;
      ctx = { action: btn.dataset.action, target: btn.dataset.target, back: btn.dataset.back, media: btn.dataset.media || "" };
      scope.value = projectID ? "project" : "";
      picker.showModal();
      load();
    });
    scope.addEventListener("change", load);
    kind.addEventListener("change", load);
    q.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(load, 250); });
  }
})();
