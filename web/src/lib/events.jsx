import { createContext, useContext, useEffect, useRef } from 'react';

const EventsContext = createContext(null);

/**
 * Maintains a single /ws/events WebSocket connection at the persistent shell
 * level with exponential-capped reconnect so route changes never drop events.
 */
export function EventsProvider({ children }) {
  const eventListeners = useRef(new Set());
  const reconnectListeners = useRef(new Set());

  useEffect(() => {
    let closed = false;
    let attempt = 0;
    let timer = 0;
    let ws = null;

    function connect() {
      if (closed) return;
      const proto = window.location.protocol === 'https:' ? 'wss://' : 'ws://';
      ws = new WebSocket(`${proto}${window.location.host}/ws/events`);

      ws.onopen = () => {
        const again = attempt > 0;
        attempt = 0;
        if (again) {
          reconnectListeners.current.forEach((fn) => fn());
        }
      };

      ws.onmessage = (ev) => {
        let msg;
        try {
          msg = JSON.parse(ev.data);
        } catch {
          return;
        }
        eventListeners.current.forEach((fn) => fn(msg));
      };

      ws.onclose = () => {
        if (closed) return;
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
  }, []);

  const registry = useRef({
    subscribe(onEvent, onReconnect) {
      if (onEvent) eventListeners.current.add(onEvent);
      if (onReconnect) reconnectListeners.current.add(onReconnect);
      return () => {
        if (onEvent) eventListeners.current.delete(onEvent);
        if (onReconnect) reconnectListeners.current.delete(onReconnect);
      };
    },
  });

  return (
    <EventsContext.Provider value={registry.current}>
      {children}
    </EventsContext.Provider>
  );
}

/**
 * Subscribe a page/component to /ws/events messages and reconnect notifications.
 */
export function useEvents(onEvent, onReconnect) {
  const bus = useContext(EventsContext);
  const eventRef = useRef(onEvent);
  const reconnectRef = useRef(onReconnect);
  eventRef.current = onEvent;
  reconnectRef.current = onReconnect;

  useEffect(() => {
    if (!bus) return undefined;
    const handleEvent = (msg) => {
      if (eventRef.current) eventRef.current(msg);
    };
    const handleReconnect = () => {
      if (reconnectRef.current) reconnectRef.current();
    };
    return bus.subscribe(handleEvent, handleReconnect);
  }, [bus]);
}
