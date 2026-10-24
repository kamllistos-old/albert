// install.js is the "Install new" tab: a debounced search across every
// source, with an Install button per result.

import { search, SessionEndedError } from './api.js';
import { h, sourceLabel, spinner } from './table.js';

const DEBOUNCE_MS = 300;
const MIN_CHARS = 2;

// installTarget is what to send to POST /api/install: mas installs by App
// Store ID, everything else by name.
export function installTarget(r) {
  return r.source === 'mas' && r.id ? r.id : r.name;
}

// initInstall builds the tab inside el. onInstall(result) starts the install
// and resolves when the job is queued. opts may have isInstalled(result) and
// isInstalling(result). It returns {focus(), refresh()}; call refresh when
// the inventory or the running jobs change.
export function initInstall(el, onInstall, opts = {}) {
  const head = h('div', 'install-head');
  head.append(
    h('h2', null, 'Install something new'),
    h('p', null, 'Search Homebrew, the App Store, npm, PyPI, Cargo and more. Pick a result and press Install.'),
  );
  const box = h('div', 'search search-big');
  const input = h('input');
  input.type = 'search';
  input.placeholder = 'Search for a tool, for example ripgrep';
  input.setAttribute('aria-label', 'Search for tools to install');
  input.autocomplete = 'off';
  input.spellcheck = false;
  const icon = document.querySelector('.search-icon').cloneNode(true);
  box.append(icon, input);
  head.append(box);

  const status = h('div', 'install-status');
  status.setAttribute('role', 'status');
  status.setAttribute('aria-live', 'polite');
  const list = h('ul', 'results');
  list.setAttribute('aria-label', 'Search results');
  el.replaceChildren(head, status, list);

  let results = [];
  let timer = 0;
  let seq = 0;
  let controller = null;
  const starting = new Set(); // results whose install request is in flight

  const key = (r) => `${r.source}/${installTarget(r)}`;

  function setStatus(text, busy) {
    status.replaceChildren();
    if (busy) status.append(spinner());
    if (text) status.append(document.createTextNode(text));
  }

  function hint() {
    setStatus('Type at least 2 letters to search.');
  }

  function button(r) {
    const b = h('button', 'btn btn-small');
    b.type = 'button';
    b.dataset.key = key(r);
    if (opts.isInstalled && opts.isInstalled(r)) {
      b.disabled = true;
      b.textContent = 'Installed';
    } else if (starting.has(key(r)) || (opts.isInstalling && opts.isInstalling(r))) {
      b.disabled = true;
      b.append(spinner(), document.createTextNode('Installing'));
    } else {
      b.className = 'btn btn-small btn-primary';
      b.textContent = 'Install';
      b.setAttribute('aria-label', `Install ${r.name} with ${sourceLabel(r.source)}`);
    }
    return b;
  }

  function render() {
    const frag = document.createDocumentFragment();
    for (const r of results) {
      const li = h('li', 'result');
      const main = h('div', 'result-main');
      const top = h('div', 'result-top');
      top.append(h('span', 'result-name', r.name), h('span', 'source-pill', sourceLabel(r.source)));
      if (r.version) top.append(h('span', 'result-version mono', r.version));
      main.append(top);
      if (r.description) main.append(h('p', 'result-desc', r.description));
      li.append(main, button(r));
      frag.append(li);
    }
    const focused = document.activeElement && list.contains(document.activeElement) ? document.activeElement.dataset.key : null;
    list.replaceChildren(frag);
    if (focused) {
      const b = list.querySelector(`button[data-key="${CSS.escape(focused)}"]`);
      if (b) b.focus({ preventScroll: true });
    }
  }

  async function run(q) {
    const mine = ++seq;
    if (controller) controller.abort();
    controller = new AbortController();
    setStatus('Searching… this can take a few seconds.', true);
    try {
      const data = await search(q, controller.signal);
      if (mine !== seq) return;
      results = (data && data.results) || [];
      setStatus(results.length
        ? `${results.length} ${results.length === 1 ? 'result' : 'results'} for "${q}"`
        : `Nothing found for "${q}". Try a different name.`);
      render();
    } catch (err) {
      if (err.name === 'AbortError' || mine !== seq) return;
      results = [];
      render();
      setStatus(err instanceof SessionEndedError ? '' : err.message);
    }
  }

  input.addEventListener('input', () => {
    clearTimeout(timer);
    const q = input.value.trim();
    if (q.length < MIN_CHARS) {
      seq++;
      if (controller) controller.abort();
      results = [];
      render();
      hint();
      return;
    }
    timer = setTimeout(() => run(q), DEBOUNCE_MS);
  });

  list.addEventListener('click', async (e) => {
    const b = e.target.closest('button[data-key]');
    if (!b || b.disabled) return;
    const r = results.find((x) => key(x) === b.dataset.key);
    if (!r) return;
    starting.add(key(r));
    render();
    try {
      await onInstall(r);
    } finally {
      starting.delete(key(r));
      render();
    }
  });

  hint();
  return {
    focus: () => input.focus(),
    refresh: render,
  };
}
