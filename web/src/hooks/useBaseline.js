import { useCallback, useEffect, useRef, useState } from 'react';
import { api } from '../lib/api';
import { useEvents } from '../lib/events';

/**
 * Live-data hook for the global Baseline items library.
 * Loads GET /api/baseline, refreshes on /ws/events reconnect, and provides
 * CRUD, duplicate, and substitution-preview operations.
 */
export function useBaseline() {
  const [items, setItems] = useState([]);
  const [noBackend, setNoBackend] = useState(false);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const refresh = useCallback(async () => {
    const r = await api('GET', '/api/baseline');
    if (!mountedRef.current) return;
    setLoading(false);
    if (r.status === 503) {
      setNoBackend(true);
      setError('');
      setItems([]);
      return;
    }
    if (!r.ok) {
      setError(r.data.error || `Could not load baseline items (${r.status})`);
      return;
    }
    setNoBackend(false);
    setError('');
    setItems(Array.isArray(r.data.items) ? r.data.items : []);
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  useEvents(null, refresh);

  const createItem = useCallback(async (payload) => {
    const r = await api('POST', '/api/baseline', payload);
    if (r.ok) {
      await refresh();
    }
    return r;
  }, [refresh]);

  const updateItem = useCallback(async (id, payload) => {
    const r = await api('PUT', `/api/baseline/${encodeURIComponent(id)}`, payload);
    if (r.ok) {
      await refresh();
    }
    return r;
  }, [refresh]);

  const duplicateItem = useCallback(async (id, name) => {
    const body = name ? { name } : {};
    const r = await api('POST', `/api/baseline/${encodeURIComponent(id)}/duplicate`, body);
    if (r.ok) {
      await refresh();
    }
    return r;
  }, [refresh]);

  const deleteItem = useCallback(async (id) => {
    const r = await api('DELETE', `/api/baseline/${encodeURIComponent(id)}`, {});
    if (r.ok) {
      await refresh();
    }
    return r;
  }, [refresh]);

  const previewTemplate = useCallback(async (payload) => {
    return api('POST', '/api/baseline/preview', payload);
  }, []);

  return {
    items,
    noBackend,
    error,
    loading,
    refresh,
    createItem,
    updateItem,
    duplicateItem,
    deleteItem,
    previewTemplate,
  };
}
