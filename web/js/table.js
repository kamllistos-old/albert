// table.js renders the installed-tools table, plus small helpers shared by
// the other modules. Rows use one delegated click listener per table.

const LABELS = {
  brew: 'Homebrew', cask: 'Homebrew apps', uv: 'uv', pipx: 'pipx', pip: 'pip',
  npm: 'npm', pnpm: 'pnpm', bun: 'Bun', mas: 'App Store', cargo: 'Cargo', go: 'Go', gem: 'RubyGems',
};
const COMMANDS = { cask: 'brew' };

// sourceLabel is a source's friendly name.
export function sourceLabel(source) {
  return LABELS[source] || source;
}

// sourceCommand is the program a source needs on the PATH.
export function sourceCommand(source) {
  return COMMANDS[source] || source;
}

export function toolKey(t) {
  return `${t.source}/${t.name}`;
}

// h creates an element with a class and optional text (always textContent).
export function h(tag, className, text) {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

export function spinner() {
  const s = h('span', 'spinner');
  s.setAttribute('aria-hidden', 'true');
  return s;
}

// toolStatus describes a tool's state in plain words.
export function toolStatus(t, provider) {
  if (t.readOnly) {
    return { label: 'Read-only', kind: 'muted', title: 'Albert can show this tool but not change it. It came with your Mac or was installed for every user.' };
  }
  if (t.outdated) return { label: 'Update available', kind: 'accent', title: `Version ${t.latest} is available.` };
  if (provider && provider.checking) return { label: 'Checking…', kind: 'muted', title: 'Albert is checking for a newer version.' };
  if (provider && provider.error) return { label: "Couldn't check", kind: 'warn', title: provider.error };
  if (t.new) return { label: 'New', kind: 'new', title: 'Installed in the last day.' };
  return { label: 'Up to date', kind: 'ok', title: 'You have the newest version.' };
}

export function badge(status) {
  const b = h('span', `badge badge-${status.kind}`, status.label);
  if (status.title) b.title = status.title;
  return b;
}

function byName(a, b) {
  return a.name.localeCompare(b.name, undefined, { sensitivity: 'base', numeric: true }) ||
    a.source.localeCompare(b.source);
}

// filterTools applies the dependency toggle, source chip and search text.
export function filterTools(snapshot, filters) {
  const q = (filters.query || '').trim().toLowerCase();
  const out = snapshot.tools.filter((t) => {
    if (t.dependency && !filters.showDeps) return false;
    if (filters.source && filters.source !== 'all' && t.source !== filters.source) return false;
    if (!q) return true;
    return t.name.toLowerCase().includes(q) ||
      (t.description || '').toLowerCase().includes(q) ||
      sourceLabel(t.source).toLowerCase().includes(q);
  });
  return out.sort(byName);
}

const COLUMNS = [
  ['Name', 'c-name'], ['Source', 'c-source'], ['Installed', 'c-ver'], ['Latest', 'c-ver'],
  ['Status', 'c-status'], ['Description', 'c-desc'], ['', 'c-act'],
];

const state = new WeakMap();

function setup(el) {
  const table = h('table', 'tools');
  const colgroup = h('colgroup');
  const thead = h('thead');
  const tr = h('tr');
  for (const [label, cls] of COLUMNS) {
    colgroup.append(h('col', cls));
    const th = h('th', cls, label);
    th.scope = 'col';
    if (!label) th.append(h('span', 'sr-only', 'Actions'));
    tr.append(th);
  }
  thead.append(tr);
  const tbody = h('tbody');
  table.append(colgroup, thead, tbody);
  const empty = h('div', 'empty');
  const s = { table, tbody, empty, byKey: new Map(), onUpdate: null, onSelect: null };

  el.addEventListener('click', (e) => {
    const action = e.target.closest('[data-action]');
    if (action && action.dataset.action === 'clear-filter') {
      s.onClear && s.onClear();
      return;
    }
    const row = e.target.closest('tr[data-key]');
    if (!row) return;
    const tool = s.byKey.get(row.dataset.key);
    if (!tool) return;
    if (action && action.dataset.action === 'update') {
      if (!action.disabled) s.onUpdate(tool);
      return;
    }
    if (e.target.closest('a')) return;
    if (!action && String(window.getSelection())) return; // the user is selecting text
    s.onSelect(tool);
  });
  state.set(el, s);
  return s;
}

function cell(cls, text, title) {
  const td = h('td', cls, text);
  if (title) td.title = title;
  return td;
}

function row(t, provider, view) {
  const key = toolKey(t);
  const tr = h('tr');
  tr.dataset.key = key;
  if (view.selected === key) tr.className = 'is-selected';

  const name = h('td', 'c-name');
  const wrap = h('div', 'name-wrap');
  const btn = h('button', 'name-btn', t.name);
  btn.type = 'button';
  btn.dataset.action = 'select';
  btn.title = `Show details for ${t.name}`;
  wrap.append(btn);
  if (t.dependency) {
    const tag = h('span', 'tag', 'Dependency');
    tag.title = 'Installed automatically because another tool needs it.';
    wrap.append(tag);
  }
  name.append(wrap);

  const latest = t.outdated ? t.latest : '—';
  const status = h('td', 'c-status');
  status.append(badge(toolStatus(t, provider)));

  const act = h('td', 'c-act');
  const up = h('button', t.outdated && !t.readOnly ? 'btn btn-small btn-primary' : 'btn btn-small');
  up.type = 'button';
  up.dataset.action = 'update';
  if (t.readOnly) {
    up.disabled = true;
    up.textContent = 'Update';
    up.title = "Albert can't update this tool.";
  } else if (view.isBusy && view.isBusy(t)) {
    up.disabled = true;
    up.append(spinner(), document.createTextNode('Updating'));
  } else {
    up.textContent = 'Update';
    up.setAttribute('aria-label', `Update ${t.name}`);
  }
  act.append(up);

  tr.append(
    name,
    cell('c-source', sourceLabel(t.source)),
    cell('c-ver mono', t.version || '—', t.version),
    cell(t.outdated ? 'c-ver mono is-newer' : 'c-ver mono is-none', latest, t.outdated ? t.latest : ''),
    status,
    cell('c-desc', t.description || '', t.description),
    act,
  );
  return tr;
}

// renderTable draws the tools that match filters. view is optional:
// {isBusy(tool), selected: key, empty: {title, text, clear}} where empty is
// shown when no row matches, and onClear is called by its button.
export function renderTable(el, snapshot, filters, onUpdate, onSelect, view = {}) {
  const s = state.get(el) || setup(el);
  s.onUpdate = onUpdate;
  s.onSelect = onSelect;
  s.onClear = view.onClear;

  // Keep keyboard focus on the same control across re-renders.
  const active = document.activeElement;
  let focusKey = null;
  let focusAction = null;
  if (active && el.contains(active) && active.dataset.action) {
    const tr = active.closest('tr[data-key]');
    if (tr) {
      focusKey = tr.dataset.key;
      focusAction = active.dataset.action;
    }
  }

  const providers = new Map(snapshot.providers.map((p) => [p.name, p]));
  const tools = filterTools(snapshot, filters);
  s.byKey = new Map(tools.map((t) => [toolKey(t), t]));

  if (tools.length === 0) {
    const e = view.empty || { title: 'Nothing to show', text: '' };
    s.empty.replaceChildren();
    if (e.loading) s.empty.append(spinner());
    s.empty.append(h('p', 'empty-title', e.title));
    if (e.text) s.empty.append(h('p', 'empty-text', e.text));
    if (e.clear) {
      const b = h('button', 'btn', e.clear);
      b.type = 'button';
      b.dataset.action = 'clear-filter';
      s.empty.append(b);
    }
    el.replaceChildren(s.empty);
    return;
  }

  const frag = document.createDocumentFragment();
  for (const t of tools) frag.append(row(t, providers.get(t.source), view));
  s.tbody.replaceChildren(frag);
  if (s.table.parentNode !== el) el.replaceChildren(s.table);

  if (focusKey) {
    const tr = s.tbody.querySelector(`tr[data-key="${CSS.escape(focusKey)}"]`);
    const target = tr && tr.querySelector(`[data-action="${focusAction}"]`);
    if (target && !target.disabled) target.focus({ preventScroll: true });
    else if (tr) tr.querySelector('.name-btn').focus({ preventScroll: true });
  }
}
