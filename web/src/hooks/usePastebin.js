import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { api } from '../lib/api';

const SAVE_DEBOUNCE_MS = 150;
const SAVE_BUSY_RETRY_MS = 100;
const SAVE_FAIL_RETRY_MS = 1000;

function newClientId() {
  return Math.random().toString(36).slice(2) + Date.now().toString(36);
}

/**
 * Shared pastebin pad: GET/POST /api/pastebin plus live updates from /ws/pastebin.
 *
 * - Edits are saved after a short debounce, tagged with a random client id so
 *   the server echo of our own write is ignored.
 * - A remote update is not applied while a local edit is unsaved or in flight,
 *   and keeps the caret/selection when the pad has focus.
 *
 * Returns `{ text, setText, status, noBackend, padRef }`. Attach `padRef` to
 * the textarea; `status` is 'connecting' | 'live' | 'saving' | 'error' |
 * 'reconnecting'.
 */
export function usePastebin() {
  const [text, setTextState] = useState('');
  const [wsState, setWsState] = useState('connecting'); // connecting | live | reconnecting
  const [saveState, setSaveState] = useState('idle'); // idle | saving | error
  const [noBackend, setNoBackend] = useState(false);

  const padRef = useRef(null);
  const clientRef = useRef(newClientId());
  const textRef = useRef('');
  const pendingRef = useRef(false); // local edit not yet confirmed by the server
  const sendingRef = useRef(false);
  const timerRef = useRef(0);
  const caretRef = useRef(null);
  const mountedRef = useRef(true);

  const push = useCallback(async () => {
    timerRef.current = 0;
    if (sendingRef.current) {
      timerRef.current = window.setTimeout(push, SAVE_BUSY_RETRY_MS);
      return;
    }
    sendingRef.current = true;
    pendingRef.current = false;
    if (mountedRef.current) setSaveState('saving');
    const r = await api('POST', '/api/pastebin', {
      text: textRef.current,
      client: clientRef.current,
    });
    sendingRef.current = false;
    if (!r.ok) {
      pendingRef.current = true;
      if (!mountedRef.current) return;
      if (r.status === 503) setNoBackend(true);
      setSaveState('error');
      // A rejected pad (too large) will not succeed on retry, but a later edit
      // reschedules; transport/server errors keep retrying.
      if (r.status !== 400) {
        timerRef.current = window.setTimeout(push, SAVE_FAIL_RETRY_MS);
      }
      return;
    }
    if (mountedRef.current && !pendingRef.current) setSaveState('idle');
  }, []);

  const setText = useCallback(
    (value) => {
      textRef.current = value;
      pendingRef.current = true;
      setTextState(value);
      if (timerRef.current) window.clearTimeout(timerRef.current);
      timerRef.current = window.setTimeout(push, SAVE_DEBOUNCE_MS);
    },
    [push]
  );

  // Apply a text coming from the server, preserving the caret when focused.
  const applyRemote = useCallback((value) => {
    if (value === textRef.current) return;
    const el = padRef.current;
    if (el && document.activeElement === el) {
      caretRef.current = { start: el.selectionStart, end: el.selectionEnd };
    }
    textRef.current = value;
    setTextState(value);
  }, []);

  useLayoutEffect(() => {
    const caret = caretRef.current;
    caretRef.current = null;
    const el = padRef.current;
    if (!caret || !el) return;
    const n = el.value.length;
    el.setSelectionRange(Math.min(caret.start, n), Math.min(caret.end, n));
  }, [text]);

  // Initial load.
  useEffect(() => {
    mountedRef.current = true;
    (async () => {
      const r = await api('GET', '/api/pastebin');
      if (!mountedRef.current) return;
      if (r.status === 503) {
        setNoBackend(true);
        return;
      }
      if (r.ok && !pendingRef.current && typeof r.data.text === 'string') {
        applyRemote(r.data.text);
      }
    })();
    return () => {
      mountedRef.current = false;
      if (timerRef.current) window.clearTimeout(timerRef.current);
      // Flush an unsaved edit so navigating away does not lose it.
      if (pendingRef.current && !sendingRef.current) {
        pendingRef.current = false;
        api('POST', '/api/pastebin', { text: textRef.current, client: clientRef.current });
      }
    };
  }, [applyRemote]);

  // Live updates.
  useEffect(() => {
    if (noBackend) return undefined;
    let closed = false;
    let attempt = 0;
    let timer = 0;
    let ws = null;

    function connect() {
      if (closed) return;
      const proto = window.location.protocol === 'https:' ? 'wss://' : 'ws://';
      ws = new WebSocket(`${proto}${window.location.host}/ws/pastebin`);
      ws.onopen = () => {
        attempt = 0;
        setWsState('live');
      };
      ws.onmessage = (ev) => {
        let m;
        try {
          m = JSON.parse(ev.data);
        } catch {
          return;
        }
        if (!m || typeof m.text !== 'string') return;
        // Our own echo, or a local edit still in flight: do not overwrite.
        if (m.client === clientRef.current || pendingRef.current || sendingRef.current) return;
        applyRemote(m.text);
      };
      ws.onclose = () => {
        if (closed) return;
        setWsState('reconnecting');
        attempt += 1;
        timer = window.setTimeout(connect, Math.min(10000, 500 * attempt));
      };
    }

    connect();
    return () => {
      closed = true;
      if (timer) window.clearTimeout(timer);
      if (ws) ws.close();
    };
  }, [noBackend, applyRemote]);

  const status = saveState !== 'idle' ? saveState : wsState;
  return { text, setText, status, noBackend, padRef };
}
