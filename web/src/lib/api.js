/**
 * Strict JSON API helper matching the panel backend contract.
 * Returns `{ ok, status, data }` without throwing on HTTP/network errors.
 */
export async function api(method, url, body) {
  const headers = { Accept: 'application/json' };
  const opts = { method, headers, credentials: 'same-origin' };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  try {
    const resp = await fetch(url, opts);
    const txt = await resp.text();
    let data = {};
    if (txt) {
      try {
        data = JSON.parse(txt);
      } catch {
        data = { error: txt || resp.statusText };
      }
    }
    return { ok: resp.ok, status: resp.status, data: data || {} };
  } catch (err) {
    return {
      ok: false,
      status: 0,
      data: { error: `Cannot reach the main server: ${err.message}` },
    };
  }
}
