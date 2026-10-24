// app.js holds the page state and wires the modules together.

import * as api from './api.js';
import {
  badge, filterTools, h, renderTable, sourceCommand, sourceLabel, toolKey, toolStatus,
} from './table.js';
import { initInstall, installTarget } from './install.js';
import { initJobs } from './jobs.js';

const $ = (id) => document.getElementById(id);

const state = {
  snapshot: null,
  filters: { query: '', source: 'all', showDeps: false },
  tab: 'installed',
  selected: null, // tool key shown in the detail panel
  active: new Map(), // unfinished jobs: id -> {source, kind, targets}
  finished: new Set(), // ids already reported done
  sessionEnded: false,
};

let es = null;
let frame = 0;
let lostTimer = 0;
let refreshTimer = 0;

const jobsUI = initJobs($('jobs'));
const installUI = initInstall($('view-install'), startInstall, {
  isInstalled: (r) => !!state.snapshot && state.snapshot.tools.some((t) =>
    t.source === r.source && (t.name === r.name || (r.id && t.id === r.id))),
  isInstalling: (r) => [...state.active.values()].some((j) =>
    j.kind === 'install' && j.source === r.source && j.targets.includes(installTarget(r))),
});

// ---- rendering ---------------------------------------------------------

function scheduleRender() {
  if (!frame) frame = requestAnimationFrame(() => { frame = 0; render(); });
}

function render() {
  renderHeader();
  if (!state.snapshot) return;
  renderChips();
  renderSummary();
  renderSourceNote();
  renderTable($('table'), state.snapshot, state.filters, startUpdate, select, {
    isBusy, selected: state.selected, empty: emptyState(), onClear: clearFilters,
  });
  renderDetail();
}

function updatableCount() {
  if (!state.snapshot) return 0;
  return state.snapshot.tools.filter((t) =>
    t.outdated && !t.readOnly && !t.dependency && t.source !== 'pip').length;
}

function renderHeader() {
  const n = updatableCount();
  const all = $('update-all');
  all.textContent = `Update all (${n})`;
  all.disabled = n === 0 || state.sessionEnded || updateAllRunning();
  all.title = n === 0 ? 'Everything that Albert can update is up to date.' : 'Update every tool that has a newer version.';
  $('checking').hidden = !(state.snapshot && state.snapshot.checkingUpdates);
}

function updateAllRunning() {
  return [...state.active.values()].some((j) => j.kind === 'update' && j.all);
}

function isScanning(p) {
  return p.available && (!p.lastScan || p.lastScan.startsWith('0001-'));
}

function chipCount(source) {
  return state.snapshot.tools.filter((t) =>
    (source === 'all' || t.source === source) && (state.filters.showDeps || !t.dependency)).length;
}

function renderChips() {
  const wrap = $('chips');
  const frag = document.createDocumentFragment();
  const chip = (source, label, count, opts = {}) => {
    const b = h('button', 'chip');
    b.type = 'button';
    b.dataset.source = source;
    b.setAttribute('aria-pressed', String(state.filters.source === source));
    b.append(h('span', null, label));
    if (count !== null) b.append(h('span', 'chip-count', String(count)));
    if (opts.warn) b.append(h('span', 'chip-warn'));
    if (opts.off) {
      b.classList.add('is-off');
      b.setAttribute('aria-disabled', 'true');
    }
    if (opts.title) b.title = opts.title;
    frag.append(b);
  };
  chip('all', 'All', chipCount('all'));
  for (const p of state.snapshot.providers) {
    const label = sourceLabel(p.name);
    if (!p.available) {
      chip(p.name, label, null, {
        off: true,
        title: `${label} isn't installed on this Mac, so there's nothing to show. (Albert couldn't find "${sourceCommand(p.name)}" on your PATH.)`,
      });
    } else if (p.error && !state.snapshot.tools.some((t) => t.source === p.name)) {
      // The source exists but could not be read at all.
      chip(p.name, label, null, { off: true, warn: true, title: `Albert couldn't read your ${label} tools: ${p.error}` });
    } else if (isScanning(p)) {
      chip(p.name, label, '…', { title: `Looking for your ${label} tools…` });
    } else {
      chip(p.name, label, chipCount(p.name), {
        warn: !!p.error,
        title: p.error ? `Albert had trouble with ${label}: ${p.error}` : `Tools installed with ${label}`,
      });
    }
  }
  wrap.replaceChildren(frag);
}

function renderSummary() {
  const tools = state.snapshot.tools.filter((t) => state.filters.showDeps || !t.dependency);
  const updates = tools.filter((t) => t.outdated && !t.readOnly).length;
  const scanning = state.snapshot.providers.some(isScanning);
  const parts = [`${tools.length} ${tools.length === 1 ? 'tool' : 'tools'}`];
  if (updates) parts.push(`${updates} with ${updates === 1 ? 'an update' : 'updates'} available`);
  else if (tools.length && !state.snapshot.checkingUpdates) parts.push('everything up to date');
  if (scanning) parts.push('still looking…');
  $('summary').textContent = parts.join(' · ');
}

function renderSourceNote() {
  const note = $('source-note');
  const p = state.snapshot.providers.find((x) => x.name === state.filters.source);
  if (p && p.available && p.error) {
    note.textContent = `Albert had trouble reading your ${sourceLabel(p.name)} tools: ${p.error}`;
    note.hidden = false;
  } else {
    note.hidden = true;
  }
}

function emptyState() {
  const f = state.filters;
  if (f.query.trim()) {
    return { title: `No tools match "${f.query.trim()}".`, text: 'Check the spelling, or look in all sources.', clear: 'Clear filter' };
  }
  const providers = state.snapshot.providers;
  if (f.source !== 'all') {
    const p = providers.find((x) => x.name === f.source);
    const label = sourceLabel(f.source);
    if (p && isScanning(p)) return { title: `Looking for your ${label} tools…`, loading: true };
    if (p && p.error) return { title: `Albert couldn't read your ${label} tools.`, text: 'Press Refresh to try again.' };
    return { title: `Nothing installed with ${label} yet.`, text: 'Use "Install new" to add something.' };
  }
  if (providers.some(isScanning)) return { title: 'Looking for your tools…', text: 'This usually takes a few seconds.', loading: true };
  if (!providers.some((p) => p.available)) {
    return { title: 'Albert found no package managers on this Mac.', text: 'Install Homebrew from brew.sh, then press Refresh.' };
  }
  return { title: 'No tools found yet.', text: 'Use "Install new" to add your first tool.' };
}

function selectedTool() {
  if (!state.selected || !state.snapshot) return null;
  return state.snapshot.tools.find((t) => toolKey(t) === state.selected) || null;
}

function renderDetail() {
  const panel = $('detail');
  const t = selectedTool();
  if (!t) {
    if (!panel.hidden) closeDetail(false);
    return;
  }
  const p = state.snapshot.providers.find((x) => x.name === t.source);
  panel.hidden = false;
  $('detail-name').textContent = t.name;
  $('detail-source').textContent = `Installed with ${sourceLabel(t.source)}${t.dependency ? ' · Dependency' : ''}`;
  $('detail-badge').replaceChildren(badge(toolStatus(t, p)));
  $('detail-desc').textContent = t.description || 'No description available.';
  $('detail-version').textContent = t.version || 'Unknown';
  $('detail-latest').textContent = t.outdated ? t.latest : (t.latest || t.version || 'Unknown');
  const home = $('detail-home');
  home.replaceChildren();
  if (/^https?:\/\//i.test(t.homepage || '')) {
    const a = h('a', null, t.homepage.replace(/^https?:\/\//i, '').replace(/\/$/, ''));
    a.href = t.homepage;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    home.append(a);
  } else {
    home.textContent = 'Not listed';
  }
  $('detail-path').textContent = t.path || 'Not known';

  const hint = $('detail-hint');
  hint.hidden = !t.readOnly;
  hint.textContent = t.readOnly ? 'Albert can show this tool but not change it. It came with your Mac or was installed for every user.' : '';
  const up = $('detail-update');
  up.hidden = t.readOnly;
  const busy = isBusy(t);
  up.disabled = busy || state.sessionEnded;
  up.textContent = busy ? 'Updating…' : (t.outdated ? `Update to ${t.latest}` : 'Update');
}

function select(t) {
  const opening = $('detail').hidden;
  state.selected = toolKey(t);
  render();
  if (opening) $('detail-close').focus({ preventScroll: true });
}

function closeDetail(restoreFocus = true) {
  const key = state.selected;
  state.selected = null;
  $('detail').hidden = true;
  if (restoreFocus && key) {
    scheduleRender();
    requestAnimationFrame(() => {
      const btn = document.querySelector(`#table tr[data-key="${CSS.escape(key)}"] .name-btn`);
      if (btn) btn.focus();
    });
  }
}

// ---- jobs ----------------------------------------------------------------

function isBusy(t) {
  for (const j of state.active.values()) {
    if (j.kind !== 'update' || j.source !== t.source) continue;
    if (j.targets.length === 0) {
      if (t.outdated) return true;
    } else if (j.targets.includes(t.name) || (t.id && j.targets.includes(t.id))) {
      return true;
    }
  }
  return false;
}

function track(job, label, extra = {}) {
  if (state.finished.has(job.id)) return; // it ended before its submit returned
  if (job.status === 'success' || job.status === 'failed') return;
  const prev = state.active.get(job.id);
  state.active.set(job.id, {
    source: job.source, kind: job.kind, targets: job.targets || [], label: label || (prev && prev.label) || '', ...extra,
  });
  jobsUI.add(job, label);
}

function announce(text) {
  $('announce').textContent = text;
}

function showError(err) {
  if (err instanceof api.SessionEndedError) return;
  notice(err.message, 'error');
}

function updateName(t) {
  // mas app names can hold spaces, which the agent refuses; its ID never does.
  return t.source === 'mas' && t.id ? t.id : t.name;
}

async function startUpdate(t) {
  const target = updateName(t);
  try {
    const res = await api.update({ source: t.source, names: [target] });
    for (const id of res.jobs) {
      track({ id, source: t.source, kind: 'update', targets: [target], status: 'queued' }, t.name);
    }
    scheduleRender();
  } catch (err) {
    showError(err);
  }
}

async function startUpdateAll() {
  const n = updatableCount();
  const pipSkipped = state.snapshot.tools.some((t) => t.source === 'pip' && t.outdated && !t.readOnly);
  const text = `This can take a few minutes. You can keep working while it runs.${pipSkipped ? ' Tools installed with pip are skipped; update them one at a time.' : ''}`;
  if (!(await confirm(`Update ${n} ${n === 1 ? 'tool' : 'tools'}?`, text, 'Update all'))) return;
  try {
    const res = await api.update({ all: true, includePip: false });
    const jobs = await Promise.all(res.jobs.map((id) => api.getJob(id).catch(() => ({ id, kind: 'update', targets: [] }))));
    for (const j of jobs) track(j, '', { all: true });
    scheduleRender();
  } catch (err) {
    showError(err);
  }
}

async function startInstall(r) {
  const target = installTarget(r);
  try {
    const res = await api.install(r.source, target);
    for (const id of res.jobs) {
      track({ id, source: r.source, kind: 'install', targets: [target], status: 'queued' }, r.name);
    }
    scheduleRender();
  } catch (err) {
    showError(err);
  }
}

const unknownJobs = new Set();

// learnJob fetches a job we only know by id (e.g. one started elsewhere).
async function learnJob(id) {
  if (state.active.has(id) || state.finished.has(id) || unknownJobs.has(id)) return;
  unknownJobs.add(id);
  try {
    track(await api.getJob(id));
    scheduleRender();
  } catch {
    // The drawer still shows its output.
  }
}

function jobDone(job) {
  state.finished.add(job.id);
  const was = state.active.get(job.id);
  state.active.delete(job.id);
  jobsUI.done(job);
  const what = (was && was.label) || ((job.targets && job.targets.length === 1) ? job.targets[0] : 'Your tools');
  if (job.status === 'success') {
    announce(job.kind === 'install' ? `${what} is installed.` : `${what} updated.`);
  } else {
    announce(`${job.kind === 'install' ? 'Installing' : 'Updating'} ${what} didn't finish. ${job.message || ''}`);
  }
  if (was || job.kind === 'install') installUI.refresh();
  scheduleRender();
}

async function syncJobs() {
  try {
    const { jobs } = await api.getJobs();
    for (const j of jobs || []) {
      const ended = j.status === 'success' || j.status === 'failed';
      if (ended && state.active.has(j.id)) jobDone(j); // it finished while we were disconnected
      else track(j, '', { all: j.kind === 'update' && !(j.targets || []).length });
    }
    scheduleRender();
  } catch (err) {
    showError(err);
  }
}

// ---- connection ----------------------------------------------------------

function notice(text, kind = 'error') {
  const n = $('notice');
  n.replaceChildren();
  if (!text) {
    n.hidden = true;
    return;
  }
  n.className = `notice notice-${kind}`;
  n.append(h('span', null, text));
  const close = h('button', 'icon-btn notice-close');
  close.type = 'button';
  close.setAttribute('aria-label', 'Dismiss');
  close.textContent = '×';
  close.addEventListener('click', () => notice(''));
  n.append(close);
  n.hidden = false;
}

function endSession() {
  if (state.sessionEnded) return;
  state.sessionEnded = true;
  if (es) es.close();
  clearTimeout(lostTimer);
  notice('');
  $('app').inert = true;
  $('session').hidden = false;
  renderHeader();
}

function connect() {
  if (state.sessionEnded) return;
  if (es) es.close();
  es = api.events({
    inventory(snap) {
      state.snapshot = snap;
      scheduleRender();
      installUI.refresh();
    },
    job({ id, line }) {
      jobsUI.line(id, line);
      learnJob(id);
    },
    jobdone: jobDone,
    open() {
      clearTimeout(lostTimer);
      if (lostTimer) {
        lostTimer = 0;
        notice('');
        syncJobs(); // catch up on anything that finished while disconnected
      }
    },
    error(closed) {
      if (closed) {
        // The agent refused the stream: find out why (a 401 ends the session).
        es.close();
        api.getTools().then(() => setTimeout(connect, 1000), () => setTimeout(connect, 5000));
      }
      if (!lostTimer) {
        lostTimer = setTimeout(() => {
          if (!state.sessionEnded) notice(`Albert isn't responding. If you stopped it, run "albert up" in Terminal to start it again.`, 'error');
        }, 3000);
      }
    },
  });
}

// ---- dialogs -------------------------------------------------------------

function confirm(title, text, ok) {
  const d = $('confirm');
  $('confirm-title').textContent = title;
  $('confirm-text').textContent = text;
  $('confirm-ok').textContent = ok;
  d.returnValue = '';
  d.showModal();
  $('confirm-ok').focus();
  return new Promise((resolve) => {
    d.addEventListener('close', () => resolve(d.returnValue === 'ok'), { once: true });
  });
}

// ---- events --------------------------------------------------------------

function setTab(tab) {
  state.tab = tab;
  const installed = tab === 'installed';
  for (const [id, on] of [['tab-installed', installed], ['tab-install', !installed]]) {
    const b = $(id);
    b.setAttribute('aria-selected', String(on));
    b.tabIndex = on ? 0 : -1;
  }
  $('view-installed').hidden = !installed;
  $('view-install').hidden = installed;
  $('filter-wrap').hidden = !installed;
  if (!installed) {
    closeDetail(false);
    installUI.focus();
  }
}

function clearFilters() {
  state.filters.query = '';
  state.filters.source = 'all';
  $('filter').value = '';
  scheduleRender();
  $('filter').focus();
}

$('tab-installed').addEventListener('click', () => setTab('installed'));
$('tab-install').addEventListener('click', () => setTab('install'));
document.querySelector('.tabs').addEventListener('keydown', (e) => {
  if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
  const next = state.tab === 'installed' ? 'install' : 'installed';
  setTab(next);
  $(next === 'installed' ? 'tab-installed' : 'tab-install').focus();
  e.preventDefault();
});

$('filter').addEventListener('input', (e) => {
  state.filters.query = e.target.value;
  scheduleRender();
});

$('show-deps').addEventListener('change', (e) => {
  state.filters.showDeps = e.target.checked;
  scheduleRender();
});

$('chips').addEventListener('click', (e) => {
  const b = e.target.closest('.chip');
  if (!b || b.getAttribute('aria-disabled') === 'true') return;
  state.filters.source = state.filters.source === b.dataset.source && b.dataset.source !== 'all' ? 'all' : b.dataset.source;
  scheduleRender();
});

$('refresh').addEventListener('click', async () => {
  const btn = $('refresh');
  btn.disabled = true;
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => { btn.disabled = false; }, 3000);
  try {
    await api.refresh();
    announce('Looking for tools and updates again.');
  } catch (err) {
    showError(err);
    btn.disabled = false;
  }
});

$('update-all').addEventListener('click', startUpdateAll);
$('detail-close').addEventListener('click', () => closeDetail());
$('detail-update').addEventListener('click', () => {
  const t = selectedTool();
  if (t) startUpdate(t);
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !$('detail').hidden && !$('confirm').open) {
    closeDetail();
    return;
  }
  const typing = e.target.closest('input, textarea, select, [contenteditable]');
  if (e.key === '/' && !typing && !e.metaKey && !e.ctrlKey && !e.altKey) {
    e.preventDefault();
    if (state.tab === 'installed') $('filter').focus();
    else installUI.focus();
  }
});

// ---- start ---------------------------------------------------------------

api.onSessionEnded(endSession);

async function start() {
  renderTable($('table'), { tools: [], providers: [] }, state.filters, startUpdate, select, {
    empty: { title: 'Opening Albert…', loading: true },
  });
  try {
    state.snapshot = await api.getTools();
  } catch (err) {
    if (err instanceof api.SessionEndedError) return;
    renderTable($('table'), { tools: [], providers: [] }, state.filters, startUpdate, select, {
      empty: { title: "Albert isn't responding.", text: 'If you stopped it, run "albert up" in Terminal to start it again. This page will pick up again once Albert is back.' },
    });
    setTimeout(start, 5000);
    return;
  }
  render();
  await syncJobs();
  connect();
}

start();
