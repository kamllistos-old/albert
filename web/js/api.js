// api.js talks to Albert's agent. "albert up" opens the page with a one-time
// code in the URL fragment (#code=...), which the browser never sends to a
// server. signIn trades it at POST /api/session for the agent's token. The
// token is kept in localStorage, which only this exact origin (scheme, host
// and port) can read, and sent as a Bearer header on every call. There are no
// cookies: browsers would send those to every port on localhost.

export const SESSION_ENDED = `Albert's session ended. Run "albert up" in Terminal to open it again.`;
const NOT_RESPONDING = `Albert isn't responding. If you stopped it, run "albert up" in Terminal to start it again.`;
const TOKEN_KEY = 'albert_token';

// SessionEndedError is thrown when the agent answers 401.
export class SessionEndedError extends Error {}

let sessionEnded = () => {};

// onSessionEnded sets the function called whenever an API call gets a 401.
export function onSessionEnded(fn) {
  sessionEnded = fn;
}

// Storage can be unavailable (private windows, blocked site data): then the
// token lives only as long as the page.
let token = '';
try {
  token = localStorage.getItem(TOKEN_KEY) || '';
} catch {
  // keep it in memory only
}

function setToken(t) {
  token = t;
  try {
    if (t) localStorage.setItem(TOKEN_KEY, t);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    // keep it in memory only
  }
}

// endSession forgets the token, which no longer works, and tells the page.
function endSession() {
  setToken('');
  sessionEnded();
}

// hasToken reports whether the page holds a token to try.
export function hasToken() {
  return token !== '';
}

function authHeaders(extra) {
  const headers = { ...extra };
  if (token) headers.Authorization = `Bearer ${token}`;
  return headers;
}

// signIn trades the one-time code in the URL fragment, if any, for the token
// and removes the fragment from the address bar and history. It throws if
// the code was refused; a token stored earlier is kept and may still work.
export async function signIn() {
  const code = new URLSearchParams(location.hash.slice(1)).get('code');
  if (!code) return;
  try {
    const data = await request('POST', '/api/session', { code });
    if (data && data.token) setToken(data.token);
  } finally {
    history.replaceState(null, '', location.pathname + location.search);
  }
}

async function request(method, path, body, signal) {
  const headers = authHeaders();
  const opts = { method, headers, credentials: 'omit', cache: 'no-store', signal };
  if (method !== 'GET') {
    headers['X-Albert-Request'] = '1';
    headers['Content-Type'] = 'application/json';
    if (body !== undefined) opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch (err) {
    if (err.name === 'AbortError') throw err;
    throw new Error(NOT_RESPONDING);
  }
  let data = null;
  try {
    data = await res.json();
  } catch {
    // Not JSON: keep data null and fall back to a generic message below.
  }
  if (res.status === 401) {
    endSession();
    throw new SessionEndedError(SESSION_ENDED);
  }
  if (!res.ok) {
    throw new Error((data && data.error) || `Albert couldn't do that (error ${res.status}).`);
  }
  return data;
}

// getTools returns the inventory snapshot: {tools, providers, checkingUpdates}.
export function getTools() {
  return request('GET', '/api/tools');
}

// refresh rescans one source, or every source, then checks for updates.
export function refresh(source) {
  const q = source ? `?source=${encodeURIComponent(source)}` : '';
  return request('POST', `/api/refresh${q}`);
}

// update takes {source, names} or {all: true, includePip} and returns {jobs: [ids]}.
export function update(req) {
  return request('POST', '/api/update', req);
}

// install installs one tool (for mas, name is the App Store ID) and returns {jobs: [id]}.
export function install(source, name) {
  return request('POST', '/api/install', { source, name });
}

// health returns {ok, version, latest}; latest is a newer Albert release or "".
export function health() {
  return request('GET', '/api/health');
}

// del deletes one installed tool and returns {jobs: [id]}.
export function del(source, name) {
  return request('POST', '/api/delete', { source, name });
}

// search looks for installable tools in every source: {results: [...]}.
export function search(q, signal) {
  return request('GET', `/api/search?q=${encodeURIComponent(q)}`, undefined, signal);
}

// getJobs returns {jobs: [...]}, newest first.
export function getJobs() {
  return request('GET', '/api/jobs');
}

// getJob returns one job.
export function getJob(id) {
  return request('GET', `/api/jobs/${encodeURIComponent(id)}`);
}

// events opens the live event stream. It uses fetch rather than EventSource
// so the Authorization header can be sent. handlers may have
// inventory(snapshot), job({id, line}), jobdone(job), open() and error().
// error is called once when the stream fails or ends, unless it was closed
// or the session ended (a 401 ends the session instead). The caller decides
// when to reconnect. It returns an object whose close() stops the stream.
export function events(handlers) {
  const ctrl = new AbortController();
  const call = (name, arg) => handlers[name] && handlers[name](arg);
  const dispatch = (block) => {
    let name = 'message';
    const data = [];
    for (const line of block.split('\n')) {
      if (line.startsWith(':')) continue; // a comment, e.g. the heartbeat
      const i = line.indexOf(':');
      const field = i < 0 ? line : line.slice(0, i);
      const value = i < 0 ? '' : line.slice(i + 1).replace(/^ /, '');
      if (field === 'event') name = value;
      else if (field === 'data') data.push(value);
    }
    if (!data.length || !['inventory', 'job', 'jobdone'].includes(name)) return;
    let parsed;
    try {
      parsed = JSON.parse(data.join('\n'));
    } catch {
      return;
    }
    call(name, parsed);
  };

  (async () => {
    try {
      const res = await fetch('/api/events', {
        headers: authHeaders({ Accept: 'text/event-stream' }),
        credentials: 'omit',
        cache: 'no-store',
        signal: ctrl.signal,
      });
      if (res.status === 401) {
        endSession();
        return;
      }
      if (!res.ok || !res.body) throw new Error(`status ${res.status}`);
      call('open');
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf('\n\n')) >= 0) {
          dispatch(buf.slice(0, i));
          buf = buf.slice(i + 2);
        }
      }
    } catch {
      // handled below
    }
    if (!ctrl.signal.aborted) call('error');
  })();

  return { close: () => ctrl.abort() };
}
