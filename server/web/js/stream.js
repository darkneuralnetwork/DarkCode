// stream.js — the live event connection.
//
// EventSource only delivers a NAMED event to a listener registered for that
// exact name, so this list is the whole contract: a type missing here is
// broadcast by the server and silently dropped by the browser. That defect
// shipped once already — file_change was emitted by every mutating tool and
// reached no renderer — so the list is derived from core.EventType and guarded
// by a Go test rather than maintained by hand here.

const TYPES = [
  'task_update', 'agent_spawn', 'agent_complete', 'tool_execution', 'model_route',
  'compression', 'final_output', 'skill_extract', 'memory_store', 'dag_update',
  'consensus', 'token_usage', 'error', 'approval', 'file_change',
  'plan_updated', 'workflow_updated', 'chat_query', 'chat_response', 'sync_gui',
];

const listeners = new Map();

/** on subscribes to one event type. Returns an unsubscribe function. */
export function on(type, fn) {
  if (!listeners.has(type)) listeners.set(type, new Set());
  listeners.get(type).add(fn);
  return () => listeners.get(type).delete(fn);
}

function emit(type, data) {
  for (const fn of listeners.get(type) || []) {
    try { fn(data); } catch (err) { console.error(`${type} handler:`, err); }
  }
  for (const fn of listeners.get('*') || []) {
    try { fn(type, data); } catch (err) { console.error('* handler:', err); }
  }
}

let source = null;

export function connect() {
  source?.close();
  source = new EventSource('/api/events');

  source.addEventListener('connected', () => emit('connected', {}));
  source.onerror = () => emit('disconnected', {});

  for (const t of TYPES) {
    source.addEventListener(t, (e) => {
      let data;
      try { data = JSON.parse(e.data); } catch { return; }
      emit(t, data);
    });
  }
}
