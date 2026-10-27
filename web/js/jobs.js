// jobs.js is the activity drawer at the bottom: one entry per update or
// install, with its live output and how it ended.

import { h, sourceLabel, spinner } from './table.js';

const MAX_OUTPUT = 200000; // characters kept per job
const STATES = {
  queued: 'Waiting to start…',
  running: 'Working…',
  success: 'Done',
  failed: "Didn't finish",
};

function title(job, label) {
  const targets = job.targets || [];
  const what = label || targets[0];
  if (job.kind === 'install') return `Installing ${what || 'a tool'}`;
  if (job.kind === 'delete') return `Deleting ${what || 'a tool'}`;
  if (targets.length === 0) return job.source ? `Updating all ${sourceLabel(job.source)} tools` : 'Updating';
  if (targets.length === 1) return `Updating ${what}`;
  return `Updating ${targets.length} tools`;
}

function finished(status) {
  return status === 'success' || status === 'failed';
}

// rank orders statuses so an entry never moves backwards (e.g. a late
// "queued" from a submit response arriving after output has started).
function rank(status) {
  if (finished(status)) return 2;
  return status === 'running' ? 1 : 0;
}

// initJobs builds the drawer inside el. It returns:
//   add(job, label)  show a job (merging into an entry that already exists);
//                    label is an optional display name for its tool
//   line(id, text)   append a line of output
//   done(job)        mark a job finished, using its full output
export function initJobs(el) {
  const bar = h('div', 'drawer-bar');
  const toggle = h('button', 'drawer-toggle');
  toggle.type = 'button';
  toggle.setAttribute('aria-expanded', 'false');
  toggle.setAttribute('aria-controls', 'jobs-body');
  const summary = h('span', 'drawer-summary');
  const chevron = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  chevron.setAttribute('viewBox', '0 0 16 16');
  chevron.setAttribute('class', 'icon chevron');
  chevron.setAttribute('aria-hidden', 'true');
  const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  path.setAttribute('d', 'M4 10l4-4 4 4');
  chevron.append(path);
  toggle.append(h('span', 'drawer-title', 'Activity'), summary, chevron);
  const clear = h('button', 'btn-link', 'Clear finished');
  clear.type = 'button';
  bar.append(toggle, clear);

  const body = h('div', 'drawer-body');
  body.id = 'jobs-body';
  const list = h('ol', 'job-list');
  body.append(list);
  el.replaceChildren(bar, body);

  const entries = new Map(); // id -> {job, label, li, ...}

  function setOpen(open) {
    el.classList.toggle('is-open', open);
    toggle.setAttribute('aria-expanded', String(open));
  }

  function updateSummary() {
    let running = 0;
    let failed = 0;
    for (const e of entries.values()) {
      if (!finished(e.job.status)) running++;
      else if (e.job.status === 'failed') failed++;
    }
    summary.replaceChildren();
    el.classList.toggle('has-failed', failed > 0 && running === 0);
    if (running) {
      summary.append(spinner(), document.createTextNode(`${running} running`));
    } else if (failed) {
      summary.textContent = failed === 1 ? '1 needs attention' : `${failed} need attention`;
    } else {
      summary.textContent = 'All done';
    }
    clear.hidden = running === entries.size;
    el.hidden = entries.size === 0;
  }

  function create(id, status) {
    const li = h('li', 'job');
    const head = h('div', 'job-head');
    const icon = h('span', 'job-icon');
    icon.setAttribute('aria-hidden', 'true');
    const name = h('span', 'job-title');
    const state = h('span', 'job-state');
    head.append(icon, name, state);
    const msg = h('p', 'job-msg');
    msg.hidden = true;
    const details = h('details', 'job-details');
    details.open = true;
    details.append(h('summary', null, 'Details'));
    const out = h('pre', 'job-out');
    const text = document.createTextNode('');
    out.append(text);
    details.append(out);
    li.append(head, msg, details);
    list.prepend(li);
    const e = { job: { id, status, targets: [] }, label: '', li, icon, name, state, msg, details, out, text };
    entries.set(id, e);
    return e;
  }

  function paint(e) {
    const s = e.job.status;
    e.li.className = `job is-${s}`;
    e.icon.replaceChildren(finished(s) ? '' : spinner());
    e.name.textContent = title(e.job, e.label);
    e.state.textContent = STATES[s] || s;
    const failMsg = s === 'failed' ? (e.job.message || 'Something went wrong. The details below show what happened.') : '';
    e.msg.textContent = failMsg;
    e.msg.hidden = !failMsg;
    e.details.hidden = e.text.data === '';
  }

  function nearBottom(pre) {
    return pre.scrollHeight - pre.scrollTop - pre.clientHeight < 24;
  }

  function setOutput(e, text) {
    if (text.length > MAX_OUTPUT) text = text.slice(-MAX_OUTPUT);
    const stick = nearBottom(e.out);
    e.text.data = text;
    if (stick) e.out.scrollTop = e.out.scrollHeight;
  }

  clear.addEventListener('click', () => {
    for (const [id, e] of entries) {
      if (finished(e.job.status)) {
        e.li.remove();
        entries.delete(id);
      }
    }
    updateSummary();
  });
  toggle.addEventListener('click', () => setOpen(!el.classList.contains('is-open')));

  return {
    add(job, label) {
      const isNew = !entries.has(job.id);
      const e = entries.get(job.id) || create(job.id, job.status || 'queued');
      const prev = e.job.status;
      e.job = { ...e.job, ...job };
      if (rank(prev) > rank(e.job.status)) e.job.status = prev; // never move a job backwards
      if (label) e.label = label;
      if (job.output && job.output.length > e.text.data.length) setOutput(e, job.output);
      paint(e);
      updateSummary();
      if (isNew) setOpen(true);
    },
    line(id, text) {
      const isNew = !entries.has(id);
      const e = entries.get(id) || create(id, 'running'); // output means it has started
      setOutput(e, `${e.text.data}${text}\n`);
      const promote = e.job.status === 'queued'; // output means it has started
      if (promote) e.job.status = 'running';
      if (isNew || promote || e.details.hidden) paint(e);
      if (isNew || promote) updateSummary();
      if (isNew) setOpen(true);
    },
    done(job) {
      const e = entries.get(job.id) || create(job.id, job.status);
      e.job = { ...e.job, ...job };
      if (job.output) setOutput(e, job.output); // the full output: lines can be dropped on the way
      if (job.status === 'success') e.details.open = false;
      paint(e);
      updateSummary();
      if (job.status === 'failed') setOpen(true);
    },
  };
}
