"use strict";
// Mac Fleet dashboard. Plain JS, no build step.
//
// Data flow: the server pushes changed hosts over SSE. The page keeps a local
// store and patches existing DOM nodes in place, so nothing flickers, scroll
// and focus survive, and values animate from old to new.

const $ = (s, r = document) => r.querySelector(s);
const app = $("#app");
const dlg = $("#dlg");
const reduce = matchMedia("(prefers-reduced-motion: reduce)").matches;

const S = {
  hosts: new Map(), loaded: false,
  range: "24h", q: "", status: "all", pool: "all", sort: { key: "name", dir: 1 },
  sel: new Set(), view: null, cmdOpen: null, lastEvent: 0,
};

// ---------- helpers ----------
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const mono = (s) => `<span class="mono">${esc(s)}</span>`;
const debounce = (fn, ms) => { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; };
const raf = (fn) => requestAnimationFrame(fn);

async function api(method, path, body) {
  const res = await fetch(path, { method, headers: body ? { "Content-Type": "application/json" } : {}, body: body ? JSON.stringify(body) : undefined });
  let data = null;
  try { data = await res.json(); } catch (_) { /* empty body */ }
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

function ago(ts) {
  if (!ts) return "never";
  const s = Math.max(0, Math.round((Date.now() - new Date(ts).getTime()) / 1000));
  if (s < 5) return "just now";
  if (s < 60) return s + "s ago";
  if (s < 3600) return Math.round(s / 60) + "m ago";
  if (s < 86400) return Math.round(s / 3600) + "h ago";
  return Math.round(s / 86400) + "d ago";
}
const agoSpan = (ts, cls = "muted") => `<span class="${cls}" data-ts="${esc(ts || "")}">${ago(ts)}</span>`;

const level = (v, w, b) => (v >= b ? "bad" : v >= w ? "warn" : "");

// Count a number up or down instead of snapping.
function setNum(el, to, fmt = (v) => Math.round(v)) {
  if (!el) return;
  const from = el._v ?? to;
  el._v = to;
  cancelAnimationFrame(el._raf);
  if (reduce || from === to || !el.isConnected) { el.textContent = fmt(to); return; }
  const t0 = performance.now(), d = 600;
  const step = (t) => {
    const k = Math.min(1, (t - t0) / d), e = 1 - Math.pow(1 - k, 3);
    el.textContent = fmt(from + (to - from) * e);
    if (k < 1) el._raf = raf(step);
  };
  el._raf = raf(step);
}
function setText(el, text) { if (el && el.textContent !== text) el.textContent = text; }
function setHTML(el, html) { if (el && el._html !== html) { el._html = html; el.innerHTML = html; } }

function flash(el) {
  if (reduce || !el.animate) return;
  el.animate([{ backgroundColor: "rgba(214,160,40,.22)" }, { backgroundColor: "rgba(214,160,40,0)" }], { duration: 1600, easing: "ease-out" });
}

function toast(msg, err) {
  const wrap = $("#toast");
  while (wrap.children.length > 3) wrap.firstChild.remove();
  const t = document.createElement("div");
  t.className = err ? "err" : "";
  t.textContent = msg;
  const bye = () => { if (!t.isConnected) return; t.classList.add("out"); setTimeout(() => t.remove(), 220); };
  t.onclick = bye;
  wrap.appendChild(t);
  setTimeout(bye, err ? 8000 : 3200);
}

// Keep a container's children in step with a list. Existing nodes are patched,
// new ones animate in, removed ones animate out, order follows the list.
function syncList(container, items, key, create, patch) {
  const have = new Map();
  [...container.children].forEach((c) => { if (c.dataset.k != null) have.set(c.dataset.k, c); });
  const keep = new Set();
  let prev = null;
  items.forEach((it) => {
    const k = String(key(it));
    keep.add(k);
    let el = have.get(k);
    if (!el) {
      const tpl = document.createElement("template");
      tpl.innerHTML = create(it).trim();
      el = tpl.content.firstElementChild;
      el.dataset.k = k;
      if (!reduce && S.loaded) el.classList.add("enter");
    }
    patch(el, it);
    const want = prev ? prev.nextElementSibling : container.firstElementChild;
    if (el !== want) container.insertBefore(el, want);
    prev = el;
  });
  have.forEach((el, k) => {
    if (keep.has(k)) return;
    if (reduce) return el.remove();
    el.classList.add("leave");
    setTimeout(() => el.remove(), 200);
  });
}

// Reorder children with a FLIP animation.
function flipOrder(container, ids, attr = "id") {
  const rows = new Map([...container.children].filter((c) => c.dataset[attr]).map((c) => [c.dataset[attr], c]));
  const before = new Map([...rows].map(([k, el]) => [k, el.getBoundingClientRect().top]));
  let changed = false;
  let prev = null;
  ids.forEach((id) => {
    const el = rows.get(id);
    if (!el) return;
    const want = prev ? prev.nextElementSibling : container.firstElementChild;
    if (el !== want) { container.insertBefore(el, want); changed = true; }
    prev = el;
  });
  if (!changed || reduce) return;
  rows.forEach((el, k) => {
    const dy = before.get(k) - el.getBoundingClientRect().top;
    if (dy && el.animate) el.animate([{ transform: `translateY(${dy}px)` }, { transform: "none" }], { duration: 320, easing: "cubic-bezier(.2,.8,.2,1)" });
  });
}

// ---------- small renderers ----------
const barHTML = (c) => `<div class="bar" data-c="${c}"><div class="track"><div class="fill"></div></div><span class="num">-</span></div>`;
function setBar(el, v, w, b) {
  const fill = el.firstElementChild.firstElementChild;
  fill.style.width = Math.min(100, Math.max(0, v)) + "%";
  fill.className = "fill " + level(v, w, b);
  setNum(el.lastElementChild, v, (x) => Math.round(x) + "%");
}
function clearBar(el) {
  el.firstElementChild.firstElementChild.style.width = "0%";
  el._lastClear = true;
  setText(el.lastElementChild, "-");
  el.lastElementChild._v = undefined;
}

function nomadBadge(h) {
  if (h.status !== "online" || !h.sample) return `<span class="badge mute">unknown</span>`;
  const n = h.sample.nomad;
  if (!n.reachable) return `<span class="badge warn">unreachable</span>`;
  if (n.draining) return `<span class="badge warn live">draining</span>`;
  if (n.status === "down") return `<span class="badge bad">down</span>`;
  if (n.eligibility === "ineligible") return `<span class="badge warn">ineligible</span>`;
  if (n.status === "ready") return `<span class="badge ok">ready</span>`;
  return `<span class="badge mute">${esc(n.status || "unknown")}</span>`;
}
const stateBadge = (s) => {
  const cls = { succeeded: "ok", running: "brand live", queued: "mute", failed: "bad", expired: "bad", halted: "bad", cancelled: "mute" }[s] || "mute";
  return `<span class="badge ${cls}">${esc(s)}</span>`;
};
const isOut = (h) => h.status === "online" && h.sample && (h.sample.nomad.draining || h.sample.nomad.eligibility === "ineligible");
const isReady = (h) => h.status === "online" && h.sample && h.sample.nomad.status === "ready" && !isOut(h);

// ---------- dialog ----------
function closeDlg() {
  if (!dlg.open) return;
  S.cmdOpen = null;
  if (reduce) return dlg.close();
  dlg.classList.add("closing");
  setTimeout(() => { dlg.classList.remove("closing"); dlg.close(); }, 160);
}
dlg.addEventListener("cancel", (e) => { e.preventDefault(); closeDlg(); });
dlg.addEventListener("mousedown", (e) => { if (e.target === dlg) closeDlg(); });

function openDialog(title, bodyHTML, onSubmit, submitLabel = "Run", danger = false) {
  dlg.innerHTML = `<form method="dialog"><h2>${esc(title)}</h2>${bodyHTML}
    <div class="actions"><button type="button" data-close>${onSubmit ? "Cancel" : "Close"}</button>${onSubmit ? `<button class="${danger ? "danger solid" : "primary"}" type="submit"><span class="lbl">${esc(submitLabel)}</span></button>` : ""}</div></form>`;
  const form = dlg.querySelector("form");
  dlg.querySelector("[data-close]").onclick = closeDlg;
  form.onsubmit = async (e) => {
    e.preventDefault();
    if (!onSubmit) return;
    const btn = form.querySelector("button[type=submit]");
    btn.disabled = true; btn.classList.add("busy");
    try {
      await onSubmit(Object.fromEntries(new FormData(form).entries()), form);
      closeDlg();
    } catch (err) {
      toast(err.message, true);
      btn.disabled = false; btn.classList.remove("busy");
      form.classList.remove("shake"); void form.offsetWidth; form.classList.add("shake");
    }
  };
  dlg.classList.remove("closing");
  dlg.showModal();
  const first = form.querySelector("input:not([type=checkbox]), textarea, select");
  if (first) first.focus();
}

const field = (label, input, id) => `<div class="field"><label for="${id}">${esc(label)}</label>${input}</div>`;
const num = (id, v, min = 1) => `<input id="${id}" name="${id}" type="number" min="${min}" value="${v}">`;
const waveFields = `${field("Hosts per wave", num("wave_size", 3), "wave_size")}${field("Halt after this many failures", num("fail_limit", 2), "fail_limit")}`;
const overrideField = `<label class="check muted"><input type="checkbox" name="override_limit"> Ignore the out-of-service limit</label>`;

// ---------- store ----------
async function loadHosts() {
  const { hosts: list } = await api("GET", "/api/hosts");
  const hosts = list || [];
  const seen = new Set();
  hosts.forEach((h) => { seen.add(h.id); S.hosts.set(h.id, h); });
  [...S.hosts.keys()].forEach((id) => { if (!seen.has(id)) S.hosts.delete(id); });
  S.loaded = true;
  updateTitle();
  return hosts;
}
function updateTitle() {
  const all = [...S.hosts.values()];
  const on = all.filter((h) => h.status === "online").length;
  const alerts = all.reduce((n, h) => n + (h.alerts || []).length, 0);
  document.title = `${alerts ? "(" + alerts + ") " : ""}Mac Fleet ${all.length ? "· " + on + "/" + all.length + " online" : ""}`;
}
const hostsArr = () => [...S.hosts.values()];

// ---------- routing ----------
function route() {
  const parts = (location.hash.replace(/^#/, "") || "/").split("/").filter(Boolean);
  const name = parts[0] === "host" ? "host" : parts[0] === "images" ? "images" : parts[0] === "settings" ? "settings" : "fleet";
  document.querySelectorAll("[data-nav]").forEach((a) => a.classList.toggle("active", a.dataset.nav === (name === "host" ? "fleet" : name)));
  const View = { fleet: FleetView, host: HostView, images: ImagesView, settings: SettingsView }[name];
  if (S.view && S.view.unmount) S.view.unmount();
  S.sel.clear();
  const v = Object.create(View);
  v.arg = parts[1];
  S.view = v;
  app.classList.remove("enter"); void app.offsetWidth; app.classList.add("enter");
  window.scrollTo(0, 0);
  Promise.resolve(v.mount()).catch((err) => {
    if (S.view === v) app.innerHTML = `<div class="card"><div class="empty">${esc(err.message)}</div></div>`;
  });
}
window.addEventListener("hashchange", route);

// =====================================================================
// Fleet
// =====================================================================
const COLS = "36px minmax(170px,1.5fr) 104px 112px minmax(120px,1fr) minmax(120px,1fr) minmax(120px,1fr) 64px 84px 88px";
const SORTS = {
  name: (a, b) => a.name.localeCompare(b.name, undefined, { numeric: true }),
  cpu: (a, b) => (b.sample ? b.sample.cpu_pct : -1) - (a.sample ? a.sample.cpu_pct : -1),
  mem: (a, b) => (b.sample ? b.sample.mem_pct : -1) - (a.sample ? a.sample.mem_pct : -1),
  disk: (a, b) => (b.sample ? b.sample.disk_used_pct : -1) - (a.sample ? a.sample.disk_used_pct : -1),
  seen: (a, b) => new Date(b.last_seen || 0) - new Date(a.last_seen || 0),
};

const FleetView = {
  mount() {
    app.innerHTML = `
    <div class="tiles">
      <div class="card tile"><div class="k">Hosts online</div><div class="v"><span data-t="online">-</span> <small>of <span data-t="total">-</span></small></div></div>
      <div class="card tile"><div class="k">Nomad ready</div><div class="v" data-t="ready">-</div></div>
      <div class="card tile"><div class="k">Draining or ineligible</div><div class="v" data-t="out">-</div></div>
      <div class="card tile"><div class="k">Average CPU</div><div class="v"><span data-t="cpu">-</span><small>%</small></div></div>
      <div class="card tile"><div class="k">Disks over threshold</div><div class="v" data-t="disk">-</div></div>
    </div>
    <section class="card"><div class="hd"><h2>Active alerts</h2><span class="muted" id="alert-n"></span></div><div id="alerts"></div></section>
    <div class="row">
      <input id="q" type="search" placeholder="Search hosts   ( / )" value="${esc(S.q)}" style="max-width:300px" aria-label="Search hosts">
      <select id="f-status" style="width:auto" aria-label="Status filter"><option value="all">Status: all</option><option value="online">Online</option><option value="offline">Offline</option></select>
      <select id="f-pool" style="width:auto" aria-label="Pool filter"></select>
      <span class="grow"></span>
      <div id="bulk"></div>
    </div>
    <section class="card table">
      <div class="t th" style="grid-template-columns:${COLS}"><span><input type="checkbox" id="sel-all" aria-label="Select all shown"></span>
        <button class="th-b" data-sort="name">Host</button><span>Agent</span><span>Nomad</span>
        <button class="th-b" data-sort="cpu">CPU</button><button class="th-b" data-sort="mem">Memory</button><button class="th-b" data-sort="disk">Disk used</button><span>VMs</span><span>Version</span><button class="th-b" data-sort="seen">Last seen</button></div>
      <div id="rows"></div>
      <div id="rows-empty" class="empty" hidden></div>
    </section>
    <section class="card" id="roll-card" hidden><div class="hd"><h2>Rollouts</h2></div><div id="rollouts"></div></section>
    <section class="card"><div class="hd"><h2>Recent commands</h2></div><div class="table" id="cmds"></div></section>`;
    $("#f-status").value = S.status;
    const rows = $("#rows");
    if (!S.loaded) rows.innerHTML = Array.from({ length: 5 }, () => `<div class="t sk"><span class="shim"></span></div>`).join("");

    $("#q").oninput = (e) => { S.q = e.target.value; this.applyFilter(); };
    $("#f-status").onchange = (e) => { S.status = e.target.value; this.applyFilter(); };
    $("#f-pool").onchange = (e) => { S.pool = e.target.value; this.applyFilter(); };
    $("#sel-all").onchange = (e) => { this.visibleRows().forEach((r) => this.select(r.dataset.id, e.target.checked)); this.renderBulk(); };
    app.querySelectorAll("[data-sort]").forEach((b) => (b.onclick = () => {
      const k = b.dataset.sort;
      S.sort = { key: k, dir: S.sort.key === k && k === "name" ? -S.sort.dir : 1 };
      this.order(true);
    }));
    rows.addEventListener("change", (e) => { const c = e.target.closest("[data-sel]"); if (c) { this.select(c.dataset.sel, c.checked); this.renderBulk(); } });

    this.cmds = []; this.batches = [];
    const loaded = S.loaded ? Promise.resolve() : loadHosts();
    this.refreshSide();
    return loaded.then(() => { if (S.view === this) this.sync(); });
  },
  async refreshSide() {
    const [c, b] = await Promise.all([api("GET", "/api/commands?limit=8"), api("GET", "/api/batches")]);
    if (S.view !== this) return;
    this.cmds = c.commands || []; this.batches = b.batches || [];
    this.renderSide();
  },
  renderSide() {
    syncList($("#cmds"), this.cmds, (c) => c.id, cmdRowHTML(true), patchCmdRow);
    if (!this.cmds.length) $("#cmds").innerHTML = `<div class="empty">No commands yet</div>`;
    const b = this.batches.slice(0, 5);
    $("#roll-card").hidden = !b.length;
    syncList($("#rollouts"), b, (x) => x.id, batchRowHTML, patchBatchRow);
  },
  onEvent(m) {
    if (m.kind === "command" || m.kind === "batch") this.refreshSideSoon();
  },
  refreshSideSoon: debounce(function () { if (S.view && S.view.refreshSide && S.view.arg === undefined) S.view.refreshSide(); }, 250),
  onHost() { this.syncSoon(); },
  onHosts() { this.sync(); },
  syncSoon() { if (this._p) return; this._p = raf(() => { this._p = 0; this.sync(); }); },
  unmount() { cancelAnimationFrame(this._p); },

  visibleRows() { return [...$("#rows").children].filter((r) => r.dataset.id && !r.hidden && !r.classList.contains("leave")); },
  select(id, on) {
    on ? S.sel.add(id) : S.sel.delete(id);
    const row = $(`#rows [data-id="${id}"]`);
    if (row) { row.classList.toggle("sel", on); const c = row.querySelector("[data-sel]"); if (c) c.checked = on; }
  },
  renderBulk() {
    const vis = this.visibleRows();
    const all = $("#sel-all");
    if (all) { all.checked = vis.length > 0 && vis.every((r) => S.sel.has(r.dataset.id)); all.indeterminate = !all.checked && vis.some((r) => S.sel.has(r.dataset.id)); }
    const el = $("#bulk");
    if (!el) return;
    if (!S.sel.size) { if (el.firstChild) { el.firstChild.classList.add("leave"); setTimeout(() => (el.innerHTML = ""), 180); } return; }
    if (!el.firstChild || el.firstChild.classList.contains("leave")) {
      el.innerHTML = `<div class="bulk enter"><span data-n></span><button data-act="bulk-drain">Drain</button><button data-act="bulk-restart">Restart Nomad</button><button data-act="bulk-pull">Pull image</button><button data-act="bulk-clear">Clear</button></div>`;
    }
    setText($("[data-n]", el), `${S.sel.size} selected`);
  },
  match(h) {
    const q = S.q.trim().toLowerCase();
    return (S.status === "all" || h.status === S.status) && (S.pool === "all" || (h.labels || {}).pool === S.pool) && (!q || h.name.toLowerCase().includes(q));
  },
  applyFilter() {
    let shown = 0;
    const hosts = S.hosts;
    [...$("#rows").children].forEach((r) => {
      const h = hosts.get(r.dataset.id);
      if (!h) return;
      const ok = this.match(h);
      if (r.hidden === ok) { r.hidden = !ok; if (ok && !reduce) r.classList.add("enter"); }
      if (ok) shown++;
    });
    const e = $("#rows-empty");
    e.hidden = shown > 0 || !S.loaded;
    e.textContent = S.hosts.size ? "No hosts match the filters" : "No hosts yet. Create an enrollment token in Settings and run the agent on a Mac.";
    this.renderBulk();
  },
  order(animate) {
    const cmp = SORTS[S.sort.key] || SORTS.name;
    const ids = hostsArr().sort((a, b) => cmp(a, b) * (S.sort.key === "name" ? S.sort.dir : 1) || SORTS.name(a, b)).map((h) => h.id);
    if (animate) flipOrder($("#rows"), ids); else {
      const rows = new Map([...$("#rows").children].map((c) => [c.dataset.id, c]));
      let prev = null;
      ids.forEach((id) => { const el = rows.get(id); if (!el) return; const want = prev ? prev.nextElementSibling : $("#rows").firstElementChild; if (el !== want) $("#rows").insertBefore(el, want); prev = el; });
    }
    app.querySelectorAll("[data-sort]").forEach((b) => b.classList.toggle("on", b.dataset.sort === S.sort.key));
  },
  sync() {
    const hosts = hostsArr();
    // tiles
    const online = hosts.filter((h) => h.status === "online");
    const avgCpu = online.length ? online.reduce((a, h) => a + (h.sample ? h.sample.cpu_pct : 0), 0) / online.length : 0;
    const diskHigh = hosts.filter((h) => h.status === "online" && (h.alerts || []).some((a) => a.text.startsWith("disk")));
    const t = (k) => $(`[data-t="${k}"]`);
    setNum(t("online"), online.length); setNum(t("total"), hosts.length); setNum(t("ready"), hosts.filter(isReady).length);
    setNum(t("out"), hosts.filter(isOut).length); setNum(t("cpu"), avgCpu); setNum(t("disk"), diskHigh.length);
    t("out").style.color = hosts.some(isOut) ? "var(--warn)" : "";
    t("disk").style.color = diskHigh.length ? "var(--bad)" : "";

    // alerts
    const alerts = hosts.flatMap((h) => h.alerts || []);
    setText($("#alert-n"), `(${alerts.length})`);
    const al = $("#alerts");
    if (!alerts.length) { if (!al.querySelector(".empty")) al.innerHTML = `<div class="empty">No active alerts</div>`; }
    else {
      al.querySelector(".empty")?.remove();
      syncList(al, alerts, (a) => a.host_id + "|" + a.text, (a) => `<div class="alert"></div>`, (el, a) =>
        setHTML(el, `<span class="dot ${a.severity === "critical" ? "bad" : "warn"} pulse"></span><span class="grow"><a href="#/host/${a.host_id}">${esc(a.host)}</a> ${esc(a.text)}</span>`));
    }

    // pools
    const pools = [...new Set(hosts.map((h) => (h.labels || {}).pool).filter(Boolean))].sort();
    const pf = $("#f-pool");
    const sig = pools.join("|");
    if (pf._sig !== sig) { pf._sig = sig; pf.innerHTML = `<option value="all">Pool: all</option>` + pools.map((p) => `<option>${esc(p)}</option>`).join(""); pf.value = pools.includes(S.pool) ? S.pool : "all"; }

    // rows
    const rows = $("#rows");
    rows.querySelectorAll(".sk").forEach((x) => x.remove());
    const before = rows.children.length;
    syncList(rows, hosts, (h) => h.id, rowHTML, patchRow);
    [...rows.children].forEach((r) => { r.dataset.id = r.dataset.k; });
    for (const id of [...S.sel]) if (!S.hosts.has(id)) S.sel.delete(id);
    if (rows.children.length !== before || !this._ordered) { this.order(false); this._ordered = true; }
    this.applyFilter();
  },
};

function rowHTML(h) {
  return `<div class="t hostrow" style="grid-template-columns:${COLS}">
    <span><input type="checkbox" data-sel="${h.id}" aria-label="Select ${esc(h.name)}"></span>
    <span><a class="mono name" href="#/host/${h.id}"></a><div class="muted pool"></div></span>
    <span class="agent"></span><span class="nomad"></span>
    <span>${barHTML("cpu")}</span><span>${barHTML("mem")}</span><span>${barHTML("disk")}</span>
    <span class="vms"></span><span class="mono muted ver"></span><span class="seen"></span></div>`;
}
function patchRow(el, h) {
  const s = h.sample, on = h.status === "online";
  const sig = JSON.stringify([on, s && s.nomad.status, s && s.nomad.draining, s && s.nomad.eligibility, s && s.nomad.reachable]);
  const first = !el._init;
  el._init = true;
  const nameEl = $(".name", el); setText(nameEl, h.name);
  setText($(".pool", el), (h.labels || {}).pool || "");
  el.classList.toggle("offline", !on);
  setHTML($(".agent", el), `<span class="dot ${on ? "ok" : "bad"}"></span> ${on ? "Online" : "Offline"}`);
  setHTML($(".nomad", el), nomadBadge(h));
  const bars = { cpu: [70, 90, s && s.cpu_pct], mem: [80, 90, s && s.mem_pct], disk: [80, 90, s && s.disk_used_pct] };
  for (const k in bars) { const b = $(`[data-c="${k}"]`, el), [w, bd, v] = bars[k]; s ? setBar(b, v, w, bd) : clearBar(b); }
  setText($(".vms", el), s ? `${s.vms_running} / 2` : "-");
  setText($(".ver", el), h.agent_version || "");
  setHTML($(".seen", el), agoSpan(h.last_seen));
  if (!first && el._sig !== sig) flash(el);
  el._sig = sig;
  const c = $("[data-sel]", el); if (c) c.checked = S.sel.has(h.id);
  el.classList.toggle("sel", S.sel.has(h.id));
}

// ----- commands & batches (shared by fleet, host, images) -----
const CMD_COLS = "1.3fr 1.2fr 110px 1.6fr 90px";
const cmdRowHTML = (withHost) => () => `<div class="t cmd" style="grid-template-columns:${withHost ? CMD_COLS : "1.3fr 110px 1.8fr 90px"};min-width:700px" data-act="cmd">
  <span class="mono type"></span>${withHost ? `<span class="muted host"></span>` : ""}<span class="st"></span><span class="muted msg"></span><span class="muted right when"></span></div>`;
function patchCmdRow(el, c) {
  el.dataset.id = c.id;
  setText($(".type", el), c.type);
  if ($(".host", el)) setText($(".host", el), c.host_name);
  setHTML($(".st", el), stateBadge(c.state));
  setText($(".msg", el), c.message || "");
  setHTML($(".when", el), agoSpan(c.created_at));
}
function batchRowHTML() {
  return `<div class="alert batch" style="flex-wrap:wrap"><div class="b-title" style="min-width:240px"></div>
    <div class="grow" style="min-width:200px"><div class="progress"><div></div></div><div class="muted b-sub" style="font-size:12px;margin-top:4px"></div></div>
    <span class="b-state"></span><span class="b-cancel"></span></div>`;
}
function patchBatchRow(el, b) {
  const total = b.host_ids.length, pct = total ? Math.round(((b.succeeded + b.failed) / total) * 100) : 0;
  setHTML($(".b-title", el), `${mono(b.type)} ${esc(b.params && b.params.image ? b.params.image : "")}<div class="muted" style="font-size:12px">${total} hosts, waves of ${b.wave_size}, wave ${b.current_wave} of ${b.total_waves}</div>`);
  const bar = $(".progress > div", el);
  bar.style.width = pct + "%";
  bar.className = b.state === "running" ? "running" : b.state === "succeeded" ? "ok" : b.state === "halted" || b.state === "failed" ? "bad" : "";
  setText($(".b-sub", el), `${b.succeeded} done, ${b.failed} failed${b.message ? ", " + b.message : ""}`);
  setHTML($(".b-state", el), stateBadge(b.state));
  setHTML($(".b-cancel", el), b.state === "running" ? `<button class="small" data-act="batch-cancel" data-id="${b.id}">Cancel</button>` : "");
}

// =====================================================================
// Charts
// =====================================================================
const RANGE_MS = { "1h": 3600e3, "24h": 86400e3, "7d": 7 * 86400e3, "30d": 30 * 86400e3 };
const GAP_MS = { "1h": 120e3, "24h": 20 * 60e3, "7d": 2 * 3600e3, "30d": 8 * 3600e3 };

function monotone(pts) {
  // pts: [[x,y],...] -> SVG path using monotone cubic interpolation (no overshoot).
  const n = pts.length;
  if (n === 0) return "";
  if (n === 1) return `M${pts[0][0].toFixed(1)},${pts[0][1].toFixed(1)}`;
  const dx = [], m = [], t = [];
  for (let i = 0; i < n - 1; i++) { dx[i] = pts[i + 1][0] - pts[i][0] || 1e-6; m[i] = (pts[i + 1][1] - pts[i][1]) / dx[i]; }
  t[0] = m[0]; t[n - 1] = m[n - 2];
  for (let i = 1; i < n - 1; i++) t[i] = m[i - 1] * m[i] <= 0 ? 0 : (m[i - 1] + m[i]) / 2;
  for (let i = 0; i < n - 1; i++) {
    if (m[i] === 0) { t[i] = 0; t[i + 1] = 0; continue; }
    const a = t[i] / m[i], b = t[i + 1] / m[i], h = Math.hypot(a, b);
    if (h > 3) { t[i] = (3 * a / h) * m[i]; t[i + 1] = (3 * b / h) * m[i]; }
  }
  let d = `M${pts[0][0].toFixed(1)},${pts[0][1].toFixed(1)}`;
  for (let i = 0; i < n - 1; i++) {
    const c = dx[i] / 3;
    d += `C${(pts[i][0] + c).toFixed(1)},${(pts[i][1] + t[i] * c).toFixed(1)} ${(pts[i + 1][0] - c).toFixed(1)},${(pts[i + 1][1] - t[i + 1] * c).toFixed(1)} ${pts[i + 1][0].toFixed(1)},${pts[i + 1][1].toFixed(1)}`;
  }
  return d;
}

class Chart {
  constructor(root, o) {
    this.o = o; this.pts = []; this.tEnd = Date.now(); this.range = "24h"; this.w = 0; this.h = 150;
    root.classList.add("chart");
    root.innerHTML = `<div class="ch"><span>${esc(o.title)}</span><b class="cv">-</b></div>
      <div class="plot"><svg role="img" aria-label="${esc(o.title)} over time"><defs><linearGradient id="g-${o.key}" x1="0" x2="0" y1="0" y2="1"><stop offset="0" stop-color="var(--brand)" stop-opacity=".22"/><stop offset="1" stop-color="var(--brand)" stop-opacity="0"/></linearGradient><clipPath id="c-${o.key}"><rect/></clipPath></defs>
        <g class="grid"></g><g clip-path="url(#c-${o.key})"><path class="area" fill="url(#g-${o.key})"/><path class="line" fill="none" stroke="var(--brand)" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/></g>
        <line class="hv" y1="0"/><circle class="hd" r="4"/></svg><div class="tip" hidden></div><div class="nodata" hidden>No data yet</div></div>`;
    this.svg = $("svg", root); this.tip = $(".tip", root); this.cv = $(".cv", root); this.nodata = $(".nodata", root);
    this.plot = $(".plot", root);
    this.ro = new ResizeObserver(() => { this.w = this.plot.clientWidth; this.draw(); });
    this.ro.observe(this.plot);
    this.plot.addEventListener("pointermove", (e) => this.hover(e));
    this.plot.addEventListener("pointerleave", () => this.unhover());
  }
  destroy() { this.ro.disconnect(); cancelAnimationFrame(this._raf); }
  set(points, range) {
    this.range = range;
    this.pts = points.map((p) => ({ t: new Date(p.t).getTime(), v: p[this.o.key] }));
    this.tEnd = Date.now();
    this.nodata.hidden = this.pts.length > 0;
    this.draw();
    this.updateValue();
  }
  push(t, v) {
    if (!["1h", "24h"].includes(this.range)) return;
    const last = this.pts[this.pts.length - 1];
    if (last && t <= last.t) return;
    this.pts.push({ t, v });
    this.nodata.hidden = true;
    this.updateValue();
    const from = this.tEnd, to = Math.max(Date.now(), t);
    cancelAnimationFrame(this._raf);
    if (reduce) { this.tEnd = to; return this.draw(); }
    const t0 = performance.now(), d = 800;
    const step = (now) => {
      const k = Math.min(1, (now - t0) / d), e = 1 - Math.pow(1 - k, 3);
      this.tEnd = from + (to - from) * e;
      this.draw();
      if (k < 1) this._raf = raf(step);
    };
    this._raf = raf(step);
  }
  updateValue() {
    const last = this.pts[this.pts.length - 1];
    if (!last) { this.cv.textContent = "-"; this.cv._v = undefined; return; }
    setNum(this.cv, last.v, this.o.fmt);
  }
  geom() {
    const w = this.w || 300, h = this.h;
    return { w, h, ml: 32, mr: 8, mt: 8, mb: 20 };
  }
  draw() {
    const g = this.geom();
    const pw = g.w - g.ml - g.mr, ph = g.h - g.mt - g.mb;
    if (pw <= 0) return;
    const span = RANGE_MS[this.range], t0 = this.tEnd - span, max = this.o.max;
    const X = (t) => g.ml + ((t - t0) / span) * pw;
    const Y = (v) => g.mt + ph - (Math.min(Math.max(v, 0), max) / max) * ph;
    this.X = X; this.Y = Y;
    this.svg.setAttribute("viewBox", `0 0 ${g.w} ${g.h}`);
    this.svg.setAttribute("width", g.w); this.svg.setAttribute("height", g.h);
    const cr = $("clipPath rect", this.svg);
    cr.setAttribute("x", g.ml); cr.setAttribute("y", 0); cr.setAttribute("width", pw); cr.setAttribute("height", g.h);

    // grid and labels
    const ticks = this.o.ticks;
    let grid = ticks.map((v) => `<line x1="${g.ml}" x2="${g.w - g.mr}" y1="${Y(v).toFixed(1)}" y2="${Y(v).toFixed(1)}"/><text x="${g.ml - 6}" y="${(Y(v) + 3.5).toFixed(1)}" text-anchor="end">${this.o.tickFmt ? this.o.tickFmt(v) : v}</text>`).join("");
    const xt = [0, 1 / 3, 2 / 3, 1].map((f) => t0 + span * f);
    grid += xt.map((t, i) => `<text x="${X(t).toFixed(1)}" y="${g.h - 5}" text-anchor="${i === 0 ? "start" : i === 3 ? "end" : "middle"}">${this.fmtX(t)}</text>`).join("");
    const gg = $(".grid", this.svg);
    if (gg._h !== grid) { gg._h = grid; gg.innerHTML = grid; }

    // line segments split at gaps
    const gap = GAP_MS[this.range];
    const segs = [];
    let cur = [];
    for (const p of this.pts) {
      if (p.t < t0 - span * 0.05) continue;
      if (cur.length && p.t - cur[cur.length - 1].t > gap) { segs.push(cur); cur = []; }
      cur.push(p);
    }
    if (cur.length) segs.push(cur);
    let line = "", area = "";
    const base = g.mt + ph;
    for (const sg of segs) {
      const xy = sg.map((p) => [X(p.t), Y(p.v)]);
      if (xy.length === 1) { line += `M${xy[0][0] - 1},${xy[0][1]}L${xy[0][0] + 1},${xy[0][1]}`; continue; }
      const d = monotone(xy);
      line += d;
      area += `${d}L${xy[xy.length - 1][0].toFixed(1)},${base}L${xy[0][0].toFixed(1)},${base}Z`;
    }
    $(".line", this.svg).setAttribute("d", line);
    $(".area", this.svg).setAttribute("d", area);
    $(".hv", this.svg).setAttribute("y2", base);
  }
  fmtX(t) {
    const d = new Date(t);
    if (this.range === "1h" || this.range === "24h") return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    return d.toLocaleDateString([], { month: "short", day: "numeric" });
  }
  hover(e) {
    if (!this.pts.length || !this.X) return;
    const r = this.plot.getBoundingClientRect(), x = e.clientX - r.left;
    let best = null, bd = 1e9;
    for (const p of this.pts) { const d = Math.abs(this.X(p.t) - x); if (d < bd) { bd = d; best = p; } }
    if (!best || bd > 40) return this.unhover();
    const px = this.X(best.t), py = this.Y(best.v);
    const hv = $(".hv", this.svg), hd = $(".hd", this.svg);
    hv.setAttribute("x1", px); hv.setAttribute("x2", px); hv.style.opacity = 1;
    hd.setAttribute("cx", px); hd.setAttribute("cy", py); hd.style.opacity = 1;
    this.tip.hidden = false;
    this.tip.innerHTML = `<b>${this.o.fmt(best.v)}</b><span>${new Date(best.t).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: this.range === "1h" ? "2-digit" : undefined })}</span>`;
    const tw = this.tip.offsetWidth;
    this.tip.style.left = Math.min(Math.max(px - tw / 2, 0), r.width - tw) + "px";
    this.tip.style.top = Math.max(py - 48, 0) + "px";
  }
  unhover() { this.tip.hidden = true; $(".hv", this.svg).style.opacity = 0; $(".hd", this.svg).style.opacity = 0; }
}

// =====================================================================
// Host detail
// =====================================================================
const HostView = {
  async mount() {
    const id = this.arg;
    this.id = id;
    app.innerHTML = `
    <div class="row"><div class="grow">
      <div class="muted"><a href="#/" style="text-decoration:none">Fleet</a> / <span data-c="crumb"></span></div>
      <div class="row" style="margin-top:4px"><h1 class="mono" data-c="name"><span class="shim" style="display:inline-block;width:180px;height:24px"></span></h1>
        <span data-c="badges" class="row" style="gap:8px"></span></div></div>
      <div class="row" data-c="actions">
        <button data-act="h-eligibility" data-c="elig">Set ineligible</button>
        <button data-act="h-restart">Restart Nomad</button><button data-act="h-nomadctl">Stop or start</button>
        <button data-act="h-config">Update config</button><button data-act="h-pull">Pull image</button>
        <button class="primary" data-act="h-drain">Drain</button></div></div>
    <div class="cols"><div class="main">
      <section class="card"><div class="hd"><h2 class="grow">Metrics</h2><span class="muted live-note" id="live-note"></span>
        <div class="seg" role="group" aria-label="Time range">${["1h", "24h", "7d", "30d"].map((r) => `<button data-act="range" data-r="${r}" class="${S.range === r ? "on" : ""}">${r}</button>`).join("")}</div></div>
        <div class="charts"><div id="ch-cpu"></div><div id="ch-mem"></div><div id="ch-disk"></div><div id="ch-vms"></div></div></section>
      <section class="card"><div class="hd"><h2>Command history</h2></div><div class="table" id="hcmds"></div></section>
    </div><aside class="side">
      <section class="card"><div class="hd"><h2>Host</h2></div><dl>
        <dt>Pool</dt><dd data-c="pool">-</dd><dt>Agent version</dt><dd class="mono" data-c="ver">-</dd><dt>Nomad node</dt><dd class="mono" data-c="node">-</dd>
        <dt>Uptime</dt><dd data-c="uptime">-</dd><dt>Cores, memory</dt><dd data-c="hw">-</dd><dt>Free on volume</dt><dd data-c="free">-</dd><dt>Load (1m)</dt><dd data-c="load">-</dd><dt>Last seen</dt><dd data-c="seen">-</dd></dl>
        <div class="bd" style="padding-top:0"><button class="danger small" data-act="h-revoke">Revoke host</button></div></section>
      <section class="card"><div class="hd"><h2 class="grow">Tart images</h2><a href="#/images">All images</a></div><div id="inv"></div></section></aside></div>`;
    const mk = (id, o) => new Chart($(id), o);
    const pct = (v) => Math.round(v) + "%";
    this.charts = {
      cpu: mk("#ch-cpu", { key: "cpu", title: "CPU", max: 100, fmt: pct, ticks: [0, 50, 100] }),
      mem: mk("#ch-mem", { key: "mem", title: "Memory", max: 100, fmt: pct, ticks: [0, 50, 100] }),
      disk: mk("#ch-disk", { key: "disk", title: "Disk used", max: 100, fmt: pct, ticks: [0, 50, 100] }),
      vms: mk("#ch-vms", { key: "vms", title: "Running VMs", max: 2, fmt: (v) => v.toFixed(1), ticks: [0, 1, 2] }),
    };
    this.cmds = [];
    const h = S.hosts.get(id);
    if (h) { this.host = h; this.paint(h); }
    const [det] = await Promise.all([api("GET", `/api/hosts/${id}`), this.loadMetrics()]);
    if (S.view !== this) return;
    S.hosts.set(id, det.host); this.host = det.host; this.paint(det.host);
    this.cmds = det.commands || []; this.paintCmds();
    this.bg = setInterval(() => { if (!document.hidden) this.loadMetrics(true); }, 60000);
  },
  unmount() { Object.values(this.charts || {}).forEach((c) => c.destroy()); clearInterval(this.bg); },
  async loadMetrics(quiet) {
    const range = S.range;
    if (!quiet) Object.values(this.charts).forEach((c) => c.plot.classList.add("loading"));
    const points = (await api("GET", `/api/hosts/${this.id}/metrics?range=${range}`)).points || [];
    if (S.view !== this || range !== S.range) return;
    const last = points[points.length - 1];
    this.lastT = last ? new Date(last.t).getTime() : 0;
    Object.values(this.charts).forEach((c) => { c.set(points, range); c.plot.classList.remove("loading"); });
    setText($("#live-note"), ["1h", "24h"].includes(range) ? "live" : "");
    $("#live-note").className = "muted live-note" + (["1h", "24h"].includes(range) ? " on" : "");
  },
  onHost(h) {
    if (h.id !== this.id) return;
    const prevAt = this.host && this.host.sample_at;
    this.host = h;
    this.paint(h);
    if (h.sample_at && h.sample_at !== prevAt && h.sample) {
      const t = new Date(h.sample_at).getTime(), s = h.sample;
      this.charts.cpu.push(t, s.cpu_pct); this.charts.mem.push(t, s.mem_pct); this.charts.disk.push(t, s.disk_used_pct); this.charts.vms.push(t, s.vms_running);
    }
  },
  onHosts() { const h = S.hosts.get(this.id); if (h) this.onHost(h); },
  onEvent(m) { if (m.kind === "command") this.refreshCmds(); },
  refreshCmds: debounce(async function () {
    const v = S.view;
    if (!v || v.id === undefined || !v.charts) return;
    const r = await api("GET", `/api/commands?host=${v.id}&limit=20`);
    if (S.view !== v) return;
    v.cmds = r.commands || []; v.paintCmds();
  }, 250),
  paintCmds() {
    const el = $("#hcmds");
    if (!this.cmds.length) { el.innerHTML = `<div class="empty">No commands yet</div>`; return; }
    el.querySelector(".empty")?.remove();
    syncList(el, this.cmds, (c) => c.id, cmdRowHTML(false), patchCmdRow);
  },
  paint(h) {
    const s = h.sample, on = h.status === "online";
    const c = (k) => $(`[data-c="${k}"]`, app);
    setText(c("crumb"), h.name); setText(c("name"), h.name);
    const alerts = (h.alerts || []).map((a) => `<span class="badge ${a.severity === "critical" ? "bad" : "warn"}">${esc(a.text)}</span>`).join(" ");
    setHTML(c("badges"), `<span class="badge ${on ? "ok" : "bad"}">${on ? "Agent online" : "Agent offline"}</span>${nomadBadge(h)} ${alerts}`);
    const ineligible = s && s.nomad.eligibility === "ineligible";
    setText(c("elig"), ineligible ? "Set eligible" : "Set ineligible");
    c("elig").dataset.eligible = ineligible ? "1" : "0";
    app.querySelectorAll("[data-c=actions] button:not(.primary)").forEach((b) => (b.disabled = !on));
    setText(c("pool"), (h.labels || {}).pool || "-");
    setText(c("ver"), h.agent_version || "-");
    setText(c("node"), s && s.nomad.node_id ? s.nomad.node_id.slice(0, 8) : "-");
    setText(c("uptime"), s ? Math.floor(s.uptime_sec / 86400) + " days" : "-");
    setText(c("hw"), `${h.cores} cores, ${Math.round(h.mem_gb)} GB`);
    s ? setNum(c("free"), s.disk_free_gb, (v) => Math.round(v) + " GB") : setText(c("free"), "-");
    s ? setNum(c("load"), s.load1, (v) => v.toFixed(2)) : setText(c("load"), "-");
    setHTML(c("seen"), agoSpan(h.last_seen, ""));
    const inv = $("#inv");
    if (!(h.inventory || []).length) { setHTML(inv, `<div class="empty">No images reported</div>`); inv._items = true; }
    else {
      if (inv._html) { inv.innerHTML = ""; inv._html = null; }
      syncList(inv, h.inventory, (im) => im.name, () => `<div class="alert"></div>`, (el, im) =>
        setHTML(el, `<span class="mono grow">${esc(im.name)}</span><span class="muted">${Math.round(im.size_gb)} GB</span>
          ${im.running ? `<span class="badge brand live">in use</span>` : `<button class="small danger" data-act="h-delimg" data-image="${esc(im.name)}">Delete</button>`}`));
    }
    state_host = h;
  },
};
let state_host = null;

// =====================================================================
// Images
// =====================================================================
const ImagesView = {
  async mount() {
    app.innerHTML = `
    <div class="row"><h1 class="grow">Tart images</h1><button data-act="img-prune">Prune unused</button><button class="primary" data-act="img-pull">Pull image</button></div>
    <section class="card table"><div class="t th" style="grid-template-columns:2fr 80px 1.4fr 90px 130px 150px"><span>Image</span><span>Size</span><span>On hosts</span><span>In use</span><span>Last used</span><span></span></div>
      <div id="imgs"><div class="t sk"><span class="shim"></span></div><div class="t sk"><span class="shim"></span></div></div></section>
    <div class="cols"><section class="card side" style="flex:1 1 480px"><div class="hd"><h2>Rollouts</h2></div><div id="irolls"></div></section>
    <section class="card side" style="flex:1 1 480px"><div class="hd"><h2>Image by host</h2><select id="matrix-image" style="width:auto;margin-left:auto" aria-label="Image"></select></div>
      <div class="bd"><div class="matrix" id="matrix"></div><div class="muted" style="margin-top:12px;font-size:13px">Filled: image present. Outline: missing. Faded: host offline.</div></div></section></div>`;
    $("#matrix-image").onchange = () => this.drawMatrix();
    await this.refresh();
  },
  async refresh() {
    const [im, bt] = await Promise.all([api("GET", "/api/images"), api("GET", "/api/batches")]);
    if (S.view !== this) return;
    const images = im.images || [], total_hosts = im.total_hosts || 0, hosts = im.hosts || [], batches = bt.batches || [];
    this.data = { images, total_hosts, hosts };
    state_images = images; state_hostsAll = hosts;
    const box = $("#imgs");
    box.querySelectorAll(".sk").forEach((x) => x.remove());
    box.querySelector(".empty")?.remove();
    if (!images.length) box.innerHTML = `<div class="empty">No images reported yet</div>`;
    syncList(box, images, (i) => i.name, () => `<div class="t" style="grid-template-columns:2fr 80px 1.4fr 90px 130px 150px">
        <span class="mono n"></span><span class="sz"></span><span class="bar"><span class="track"><span class="fill"></span></span><span class="num" style="width:56px"></span></span>
        <span class="use"></span><span class="muted last"></span><span class="row act" style="gap:6px;justify-content:flex-end"></span></div>`, (el, im) => {
      setText($(".n", el), im.name); setText($(".sz", el), Math.round(im.size_gb) + " GB");
      $(".fill", el).style.width = (total_hosts ? (im.host_ids.length / total_hosts) * 100 : 0) + "%";
      setText($(".num", el), `${im.host_ids.length}/${total_hosts}`);
      setText($(".use", el), `${im.running_vms} VMs`);
      setHTML($(".last", el), im.last_accessed ? agoSpan(im.last_accessed) : "-");
      setHTML($(".act", el), `<button class="small" data-act="img-pull" data-image="${esc(im.name)}">Roll out</button><button class="small danger" data-act="img-delete" data-image="${esc(im.name)}">Delete</button>`);
    });
    const rb = batches.filter((b) => b.type.startsWith("tart."));
    const rc = $("#irolls");
    rc.querySelector(".empty")?.remove();
    if (!rb.length) rc.innerHTML = `<div class="empty">No rollouts yet</div>`;
    syncList(rc, rb, (b) => b.id, batchRowHTML, patchBatchRow);
    const sel = $("#matrix-image"), keep = sel.value;
    const names = images.map((i) => i.name);
    if (sel._sig !== names.join("|")) { sel._sig = names.join("|"); sel.innerHTML = names.map((n) => `<option>${esc(n)}</option>`).join(""); if (names.includes(keep)) sel.value = keep; }
    sel.hidden = !names.length;
    this.drawMatrix();
  },
  drawMatrix() {
    if (!this.data) return;
    const { images, hosts } = this.data;
    const im = images.find((i) => i.name === $("#matrix-image").value);
    $("#matrix").innerHTML = hosts.map((h) => `<div class="cell ${im && im.host_ids.includes(h.id) ? "have" : ""} ${h.status !== "online" ? "off" : ""}" title="${esc(h.name)}"></div>`).join("");
  },
  soon: debounce(function () { if (S.view && S.view.data !== undefined && S.view.refresh && S.view.drawMatrix) S.view.refresh(); }, 600),
  onEvent(m) { if (m.kind === "batch" || m.kind === "images" || m.kind === "command") this.soon(); },
  onHost() { this.soon(); },
  onHosts() {},
};
let state_images = [], state_hostsAll = [];

// =====================================================================
// Settings (static page, no live patching)
// =====================================================================
function codeBlock(id, text, dim) {
  return `<div class="code${dim ? " dim" : ""}"><pre id="${id}">${esc(text)}</pre><button class="small" data-act="copy" data-from="${id}">Copy</button></div>`;
}

const SettingsView = {
  async mount() {
    const [st, au, ins] = await Promise.all([api("GET", "/api/settings"), api("GET", "/api/audit"), api("GET", "/api/agent-install")]);
    if (S.view !== this) return;
    const entries = au.entries || [];
    this.known = new Set(S.hosts.keys());
    this.ins = ins;
    app.innerHTML = `<h1>Settings</h1>
    <section class="card"><div class="hd"><h2 class="grow">Nomad connection</h2><span id="nomad-state" class="badge mute">${st.nomad_token_set ? "Token set" : "No token"}</span></div>
      <form class="bd" id="f-nomad">
        ${field("Nomad API address", `<input id="nomad_addr" type="text" value="${esc(st.nomad_addr)}" placeholder="https://nomad.example.com:4646">`, "nomad_addr")}
        ${field("ACL token", `<input id="nomad_token" type="password" autocomplete="off" placeholder="${st.nomad_token_set ? "Paste a new token to replace the current one" : "Paste a token"}">`, "nomad_token")}
        <div class="muted" style="margin-bottom:12px">The token is stored encrypted and is never shown again.${st.nomad_token_updated ? " Last replaced " + ago(st.nomad_token_updated) + "." : ""}</div>
        <div class="row"><button type="button" data-act="nomad-test"><span class="lbl">Test connection</span></button><button class="primary" type="submit"><span class="lbl">Save</span></button></div></form></section>
    <section class="card"><div class="hd"><h2>Thresholds</h2></div>
      <form class="bd" id="f-thresholds"><div class="tiles">
        ${field("Disk used above (%)", num("disk_pct", st.disk_pct), "disk_pct")}${field("Memory used above (%)", num("mem_pct", st.mem_pct), "mem_pct")}
        ${field("Agent offline after (minutes)", num("offline_min", st.offline_min), "offline_min")}${field("Max hosts out of service (%)", num("max_out_pct", st.max_out_pct), "max_out_pct")}</div>
        <button class="primary" type="submit"><span class="lbl">Save thresholds</span></button></form></section>
    <section class="card"><div class="hd"><h2 class="grow">Install the agent on a Mac</h2><span id="enroll-state" class="badge mute">Waiting</span></div><div class="bd steps">
      <p class="muted" style="margin:0">Run these on the Mac as the same non-root user that runs Nomad and Tart. Agent builds come from ${ins.private ? "the private" : "the public"} GitHub repo <a href="https://github.com/${esc(ins.repo)}" target="_blank" rel="noopener">${esc(ins.repo)}</a>.</p>
      ${ins.private ? `<div class="step"><span class="n">1</span><div class="grow"><b>Give the Mac read access to the repo</b>
        <div class="muted">Create a fine-grained GitHub token with read-only Contents access to ${esc(ins.repo)}, then export it in the shell you will use. It is only used to download the agent and is not stored.</div>
        ${codeBlock("code-token", "export GITHUB_TOKEN=<your-read-only-token>")}</div></div>` : ""}
      <div class="step"><span class="n">${ins.private ? 2 : 1}</span><div class="grow"><b>Create a one-time enrollment token</b>
        <div class="muted">It works once and expires in 24 hours. The pool label is optional.</div>
        <div class="row" style="margin-top:8px"><input id="enroll-pool" type="text" placeholder="Pool label (optional)" style="max-width:260px"><button class="primary" data-act="enroll"><span class="lbl">Create token and command</span></button></div></div></div>
      <div class="step"><span class="n">${ins.private ? 3 : 2}</span><div class="grow"><b>Run the install command</b>
        <div class="muted">Downloads the agent, verifies its checksum, enrolls this Mac and starts it as a LaunchAgent.</div>
        <div id="enroll-out">${codeBlock("code-install", ins.install_command, true)}</div></div></div>
      <div class="step"><span class="n">${ins.private ? 4 : 3}</span><div class="grow"><b>Confirm</b>
        <div class="muted">The Mac appears under <a href="#/">Fleet</a> within a few seconds. This page tells you when it connects.</div></div></div>
      <details class="more"><summary>Downloads, upgrade and manual install</summary>
        <div class="dl">
          <a href="${esc(ins.releases_url)}" target="_blank" rel="noopener">All releases</a>
          <a href="${esc(ins.downloads.darwin_arm64)}" target="_blank" rel="noopener">macfleet-agent-darwin-arm64</a>
          <a href="${esc(ins.downloads.darwin_amd64)}" target="_blank" rel="noopener">macfleet-agent-darwin-amd64</a>
          <a href="${esc(ins.downloads.checksums)}" target="_blank" rel="noopener">SHA256SUMS</a>
        </div>
        ${ins.private ? `<div class="muted" style="margin-top:6px">These links need a GitHub login with access to the repo.</div>` : ""}
        <div style="margin-top:12px"><b>Upgrade an enrolled Mac</b><div class="muted">Replaces the binary and restarts the agent; enrollment is kept.</div>${codeBlock("code-upgrade", ins.upgrade_command)}</div>
        <div style="margin-top:12px"><b>Server details</b><div class="muted">Agent endpoint ${mono(ins.server)}, CA SHA-256 ${mono(ins.ca_sha256)}</div></div>
      </details></div></section>
    <section class="card"><div class="hd"><h2>Audit log</h2></div><div class="table">${entries.length ? entries.slice(0, 30).map((e) => `<div class="t" style="grid-template-columns:130px 160px 1fr 1fr;min-width:700px">${agoSpan(e.ts)}<span>${esc(e.actor)}</span>${mono(e.action)}<span class="muted">${esc(e.target)}</span></div>`).join("") : `<div class="empty">Nothing yet</div>`}</div></section>`;
    const read = (id) => document.getElementById(id).value;
    const saveAll = async () => {
      const cur = await api("GET", "/api/settings");
      return api("PUT", "/api/settings", {
        nomad_addr: read("nomad_addr").trim(), nomad_token: read("nomad_token"),
        disk_pct: +read("disk_pct"), mem_pct: +read("mem_pct"), offline_min: +read("offline_min"), max_out_pct: +read("max_out_pct"),
        target_agent_version: cur.target_agent_version,
      });
    };
    const submit = (id, after) => ($(id).onsubmit = async (e) => {
      e.preventDefault();
      const b = e.submitter || $("button[type=submit]", e.target);
      b.classList.add("busy");
      try { await saveAll(); toast("Saved"); after && after(); } catch (err) { toast(err.message, true); } finally { b.classList.remove("busy"); }
    });
    submit("#f-nomad", () => { const t = $("#nomad_token"); if (t.value) { setText($("#nomad-state"), "Token set"); t.value = ""; t.placeholder = "Paste a new token to replace the current one"; } });
    submit("#f-thresholds");
  },
  onEvent() {},
  onHost(h) {
    if (!this.known || this.known.has(h.id)) return;
    this.known.add(h.id);
    const st = $("#enroll-state");
    if (st) { st.className = "badge ok"; st.textContent = "Connected"; }
    toast(`${h.name} enrolled and connected`);
  },
  onHosts() {},
};

// =====================================================================
// Actions
// =====================================================================
const acts = {
  "bulk-clear": () => { [...S.sel].forEach((id) => S.view.select(id, false)); S.view.renderBulk(); },
  range: (el) => {
    S.range = el.dataset.r;
    app.querySelectorAll("[data-act=range]").forEach((b) => b.classList.toggle("on", b === el));
    S.view.loadMetrics().catch((e) => toast(e.message, true));
  },
  cmd: (el) => openCommand(el.dataset.id || el.closest("[data-id]").dataset.id),
  "batch-cancel": async (el) => { await api("POST", `/api/batches/${el.dataset.id}/cancel`); toast("Cancelled"); },

  "bulk-drain": () => bulkDialog("Drain selected hosts", "nomad.drain", field("Drain deadline (minutes, 0 for none)", num("deadline_minutes", 30, 0), "deadline_minutes"), (f) => ({ deadline_minutes: +f.deadline_minutes })),
  "bulk-restart": () => bulkDialog("Restart Nomad on selected hosts", "nomad.restart", "", () => ({})),
  "bulk-pull": () => bulkDialog("Pull an image on selected hosts", "tart.pull", field("Image", `<input id="image" name="image" type="text" required placeholder="registry/name:tag">`, "image"), (f) => ({ image: f.image.trim() })),

  "h-drain": () => {
    const h = state_host;
    openDialog(`Drain ${h.name}`, `<p class="muted">Marks the node ineligible and migrates its allocations.</p>
      ${field("Deadline (minutes, 0 for none)", num("deadline_minutes", 30, 0), "deadline_minutes")}
      <label class="check"><input type="checkbox" name="force"> Force drain immediately</label>
      ${field("Type the host name to confirm", `<input id="confirm" name="confirm" type="text" autocomplete="off" placeholder="${esc(h.name)}">`, "confirm")}${overrideField}`,
      async (f) => { if (f.confirm !== h.name) throw new Error("Host name does not match"); await hostCmd(h.id, "nomad.drain", { deadline_minutes: +f.deadline_minutes, force: !!f.force }, !!f.override_limit); }, "Start drain", true);
  },
  "h-eligibility": (el) => {
    const h = state_host, toEligible = el.dataset.eligible === "1";
    openDialog(`${toEligible ? "Set eligible" : "Set ineligible"}: ${h.name}`, `<p class="muted">${toEligible ? "The node will receive new allocations again." : "The node keeps running allocations but receives no new ones."}</p>${toEligible ? "" : overrideField}`,
      async (f) => hostCmd(h.id, "nomad.eligibility", { eligible: toEligible }, !!f.override_limit), "Confirm");
  },
  "h-restart": () => confirmHost("Restart Nomad", "nomad.restart", {}, "The client restarts and the agent waits for it to rejoin as ready."),
  "h-nomadctl": () => {
    const h = state_host;
    openDialog(`Stop or start Nomad: ${h.name}`, `${field("Action", `<select id="op" name="op"><option value="nomad.stop">Stop the client</option><option value="nomad.start">Start the client</option></select>`, "op")}
      <p class="muted">Drain first if allocations are running.</p>${field("Type the host name to confirm", `<input id="confirm" name="confirm" type="text" autocomplete="off" placeholder="${esc(h.name)}">`, "confirm")}${overrideField}`,
      async (f) => { if (f.confirm !== h.name) throw new Error("Host name does not match"); await hostCmd(h.id, f.op, {}, !!f.override_limit); }, "Run", true);
  },
  "h-config": () => {
    const h = state_host;
    openDialog(`Update Nomad config: ${h.name}`, `<p class="muted">The agent validates the file, keeps the previous one as a backup, and rolls back if the node does not return ready.</p>
      ${field("Client config (HCL)", `<textarea id="content" name="content" required spellcheck="false"></textarea>`, "content")}<label class="check"><input type="checkbox" name="restart" checked> Restart Nomad after writing</label>${overrideField}`,
      async (f) => hostCmd(h.id, "nomad.config.apply", { content: f.content, restart: !!f.restart }, !!f.override_limit), "Apply");
  },
  "h-pull": () => {
    const h = state_host;
    openDialog(`Pull image on ${h.name}`, field("Image", `<input id="image" name="image" type="text" required placeholder="registry/name:tag">`, "image"), async (f) => hostCmd(h.id, "tart.pull", { image: f.image.trim() }), "Pull");
  },
  "h-delimg": (el) => confirmHost(`Delete ${el.dataset.image}`, "tart.delete", { image: el.dataset.image }, "Deletes the image from this host. Images in use by a running VM are refused.", true),
  "h-revoke": () => {
    const h = state_host;
    openDialog(`Revoke ${h.name}`, `<p class="muted">The agent is disconnected and its certificate stops working. Enroll again to bring the host back.</p>`,
      async () => { await api("POST", `/api/hosts/${h.id}/revoke`); S.hosts.delete(h.id); location.hash = "#/"; }, "Revoke", true);
  },

  "img-pull": (el) => {
    const image = el.dataset.image || "";
    openDialog(image ? `Roll out ${image}` : "Pull an image", `${field("Image", `<input id="image" name="image" type="text" required value="${esc(image)}" placeholder="registry/name:tag">`, "image")}
      ${field("Target", `<select id="target" name="target"><option value="missing">Online hosts that do not have it</option><option value="all">All online hosts</option></select>`, "target")}${waveFields}`,
      async (f) => {
        const img = f.image.trim();
        const have = (state_images.find((i) => i.name === img) || { host_ids: [] }).host_ids;
        const ids = state_hostsAll.filter((h) => h.status === "online" && (f.target === "all" || !have.includes(h.id))).map((h) => h.id);
        if (!ids.length) throw new Error("No hosts to roll out to");
        await api("POST", "/api/batches", { type: "tart.pull", host_ids: ids, params: { image: img }, wave_size: +f.wave_size, fail_limit: +f.fail_limit });
        toast(`Rolling out to ${ids.length} hosts`);
      }, "Start rollout");
  },
  "img-delete": (el) => {
    const image = el.dataset.image, im = state_images.find((i) => i.name === image);
    openDialog(`Delete ${image}`, `<p class="muted">Deletes the image from the ${im.host_ids.length} hosts that have it. Hosts running a VM from it are skipped with an error.</p>${waveFields}
      ${field("Type the image name to confirm", `<input id="confirm" name="confirm" type="text" autocomplete="off">`, "confirm")}`,
      async (f) => { if (f.confirm !== image) throw new Error("Image name does not match"); await api("POST", "/api/batches", { type: "tart.delete", host_ids: im.host_ids, params: { image }, wave_size: +f.wave_size, fail_limit: +f.fail_limit }); }, "Delete", true);
  },
  "img-prune": () => {
    openDialog("Prune unused images", `<p class="muted">Removes images that no VM is using and that were last used before the cutoff, on all online hosts.</p>
      ${field("Not used for (days)", num("days", 14), "days")}<label class="check"><input type="checkbox" name="dry" checked> Dry run, only report what would go</label>${waveFields}`,
      async (f) => {
        const ids = state_hostsAll.filter((h) => h.status === "online").map((h) => h.id);
        await api("POST", "/api/batches", { type: "tart.prune", host_ids: ids, params: { older_than_days: +f.days, dry_run: !!f.dry }, wave_size: +f.wave_size, fail_limit: +f.fail_limit });
      }, "Run");
  },

  "nomad-test": async (el) => {
    el.classList.add("busy");
    try {
      const cur = await api("GET", "/api/settings");
      await api("PUT", "/api/settings", { ...cur, nomad_addr: $("#nomad_addr").value.trim(), nomad_token: $("#nomad_token").value });
      const r = await api("POST", "/api/settings/nomad/test");
      toast(r.ok ? "Connected to Nomad" : "Failed: " + r.error, !r.ok);
    } catch (err) { toast(err.message, true); } finally { el.classList.remove("busy"); }
  },
  enroll: async (el) => {
    const pool = $("#enroll-pool").value.trim();
    el.classList.add("busy");
    try {
      const r = await api("POST", "/api/enroll-tokens", { labels: pool ? { pool } : {} });
      const out = $("#enroll-out");
      out.innerHTML = codeBlock("code-install", r.install_command) + `<div class="muted" style="margin-top:6px">Valid for 24 hours, works once.</div>`;
      out.classList.remove("enter"); void out.offsetWidth; out.classList.add("enter");
      const st = $("#enroll-state"); st.className = "badge brand live"; st.textContent = "Waiting for the agent";
    } finally { el.classList.remove("busy"); }
  },
  copy: async (el) => {
    const text = document.getElementById(el.dataset.from).textContent;
    try { await navigator.clipboard.writeText(text); }
    catch (_) { const r = document.createRange(); r.selectNodeContents(document.getElementById(el.dataset.from)); const s = getSelection(); s.removeAllRanges(); s.addRange(r); document.execCommand("copy"); s.removeAllRanges(); }
    el.textContent = "Copied"; el.classList.add("done");
    setTimeout(() => { el.textContent = "Copy"; el.classList.remove("done"); }, 1400);
  },
};

// Command detail dialog that follows the command while it runs.
async function openCommand(id) {
  const { command: c, events } = await api("GET", `/api/commands/${id}`);
  S.cmdOpen = id;
  openDialog(c.type, `<p class="muted" id="cmd-head"></p><pre class="log" id="cmd-log" style="border-radius:6px"></pre>`, null);
  paintCommand(c, events);
}
function paintCommand(c, events) {
  const head = $("#cmd-head"), log = $("#cmd-log");
  if (!head) return;
  head.innerHTML = `${esc(c.host_name)} · ${stateBadge(c.state)} ${esc(c.message)}`;
  const text = events.length ? events.map((e) => new Date(e.ts).toLocaleTimeString() + "  " + e.line).join("\n") : "No output yet";
  if (log.textContent !== text) {
    const stick = log.scrollTop + log.clientHeight >= log.scrollHeight - 24;
    log.textContent = text;
    if (stick) log.scrollTop = log.scrollHeight;
  }
}
const refreshOpenCommand = debounce(async () => {
  if (!S.cmdOpen || !dlg.open) return;
  try { const { command, events } = await api("GET", `/api/commands/${S.cmdOpen}`); paintCommand(command, events); } catch (_) { /* ignore */ }
}, 200);

async function hostCmd(hostId, type, params, override) {
  const c = await api("POST", `/api/hosts/${hostId}/commands`, { type, params, override_limit: !!override });
  toast("Command sent");
  const v = S.view;
  if (v && v.cmds && c && c.id) { v.cmds = [c, ...v.cmds.filter((x) => x.id !== c.id)]; v.paintCmds && v.paintCmds(); }
  return c;
}
function confirmHost(title, type, params, text, danger) {
  const h = state_host;
  openDialog(`${title}: ${h.name}`, `<p class="muted">${esc(text)}</p>${type === "nomad.restart" ? overrideField : ""}`, async (f) => hostCmd(h.id, type, params, !!f.override_limit), "Confirm", !!danger);
}
function bulkDialog(title, type, extraFields, toParams) {
  const n = S.sel.size;
  openDialog(title, `<p class="muted">${n} hosts. Each wave must finish before the next one starts.</p>${extraFields}${waveFields}${type.startsWith("nomad") ? overrideField : ""}`,
    async (f) => {
      await api("POST", "/api/batches", { type, host_ids: [...S.sel], params: toParams(f), wave_size: +f.wave_size, fail_limit: +f.fail_limit, override_limit: !!f.override_limit });
      [...S.sel].forEach((id) => S.view.select(id, false)); S.view.renderBulk(); toast("Started");
    });
}

document.addEventListener("click", (e) => {
  const el = e.target.closest("[data-act]");
  if (!el || el.disabled) return;
  const fn = acts[el.dataset.act];
  if (fn) Promise.resolve(fn(el)).catch((err) => toast(err.message, true));
});
document.addEventListener("keydown", (e) => {
  if (e.key === "/" && !/input|textarea|select/i.test(document.activeElement.tagName) && $("#q")) { e.preventDefault(); $("#q").focus(); }
});

// =====================================================================
// Live updates
// =====================================================================
const liveDot = $("#live-dot"), liveText = $("#live-text"), liveAgo = $("#live-ago");
let wasDown = false;

function ping() {
  S.lastEvent = Date.now();
  if (reduce || !liveDot.animate) return;
  liveDot.animate([{ boxShadow: "0 0 0 0 rgba(31,157,99,.55)" }, { boxShadow: "0 0 0 8px rgba(31,157,99,0)" }], { duration: 700, easing: "ease-out" });
}

function connectEvents() {
  const es = new EventSource("/api/events");
  es.onopen = () => {
    liveDot.className = "dot ok"; liveText.textContent = "Live";
    if (wasDown) { wasDown = false; resync(); }
  };
  es.onerror = () => { wasDown = true; liveDot.className = "dot warn"; liveText.textContent = "Reconnecting"; };
  es.onmessage = (e) => {
    let m; try { m = JSON.parse(e.data); } catch (_) { return; }
    ping();
    if (m.kind === "host") {
      if (m.host) { S.hosts.set(m.host.id, m.host); updateTitle(); S.view && S.view.onHost && S.view.onHost(m.host); }
      else resyncHosts();
    } else {
      if (m.kind === "command") refreshOpenCommand();
      S.view && S.view.onEvent && S.view.onEvent(m);
    }
  };
}
const resyncHosts = debounce(async () => { try { await loadHosts(); S.view && S.view.onHosts && S.view.onHosts(); } catch (_) { /* retry on next tick */ } }, 300);
function resync() {
  resyncHosts();
  if (S.view && S.view.refreshSide) S.view.refreshSide();
  if (S.view && S.view.refresh) S.view.refresh();
}

// Relative times tick every second without touching the network.
setInterval(() => {
  if (document.hidden) return;
  document.querySelectorAll("[data-ts]").forEach((el) => { const t = ago(el.dataset.ts || null); if (el.textContent !== t) el.textContent = t; });
  if (S.lastEvent) liveAgo.textContent = "updated " + ago(S.lastEvent);
}, 1000);
// Host status flips (offline after silence) have no event, so resync quietly.
setInterval(() => { if (!document.hidden) resyncHosts(); }, 20000);
document.addEventListener("visibilitychange", () => { if (!document.hidden) resync(); });

connectEvents();
route();
