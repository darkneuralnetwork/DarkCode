// api.js — every network call the interface makes, in one file.
//
// The previous UI spread `fetch(API + ...)` across twenty-nine files with
// inconsistent error handling, so a failing endpoint surfaced differently
// depending on which screen you were on. One helper, one failure shape.

/** get fetches JSON, throwing with the server's own message when it can. */
export async function get(path) {
  const res = await fetch(path, { headers: { Accept: 'application/json' } });
  const body = await res.json().catch(() => null);
  if (!res.ok) throw new Error(body?.error || `${res.status} ${res.statusText}`);
  return body;
}

/** post sends JSON. Same failure shape as get. */
export async function post(path, data, signal) {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(data ?? {}),
    signal,
  });
  const body = await res.json().catch(() => null);
  if (!res.ok) throw new Error(body?.error || `${res.status} ${res.statusText}`);
  return body;
}

/** fmtNum keeps long counts readable in a fixed-width rail. */
export function fmtNum(n) {
  n = Number(n) || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}

export function fmtCost(n) {
  n = Number(n) || 0;
  return n === 0 ? '$0.00' : '$' + n.toFixed(n < 0.01 ? 4 : 2);
}

/** esc is the only place HTML is escaped, so there is one thing to audit. */
export function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}
