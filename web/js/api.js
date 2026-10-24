// api.js talks to Albert's agent. The browser is signed in with an HttpOnly
// cookie set by /login, so no token is ever handled here.

export const SESSION_ENDED = `Albert's session ended. Run "albert up" in Terminal to open it again.`;

// SessionEndedError is thrown when the agent answers 401.
export class SessionEndedError extends Error {}

let sessionEnded = () => {};

// onSessionEnded sets the function called whenever an API call gets a 401.
export function onSessionEnded(fn) {
  sessionEnded = fn;
}

async function request(method, path, body, signal) {
  const opts = { method, headers: {}, credentials: 'same-origin', cache: 'no-store', signal };
  if (method !== 'GET') {
    opts.headers['X-Albert-Request'] = '1';
    opts.headers['Content-Type'] = 'application/json';
    if (body !== undefined) opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch (err) {
    if (err.name === 'AbortError') throw err;
    throw new Error(`Albert isn't responding. If you stopped it, run "albert up" in Terminal to start it again.`);
  }
  let data = null;
  try {
    data = await res.json();
  } catch {
    // Not JSON: keep data null and fall back to a generic message below.
  }
  if (res.status === 401) {
    sessionEnded();
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

// events opens the live event stream. handlers may have inventory(snapshot),
// job({id, line}), jobdone(job), open() and error(closed). It returns the
// EventSource so the caller can close it.
export function events(handlers) {
  const es = new EventSource('/api/events');
  const on = (name) => (e) => {
    let data;
    try {
      data = JSON.parse(e.data);
    } catch {
      return;
    }
    if (handlers[name]) handlers[name](data);
  };
  es.addEventListener('inventory', on('inventory'));
  es.addEventListener('job', on('job'));
  es.addEventListener('jobdone', on('jobdone'));
  es.addEventListener('open', () => handlers.open && handlers.open());
  es.addEventListener('error', () => handlers.error && handlers.error(es.readyState === EventSource.CLOSED));
  return es;
}
