/* 05-eventbus.js — the one place SSE events fan out from.
 *
 * Before this, a module that wanted to react to live events did it by
 * wrapping window.addEvent: capture whatever function currently holds that
 * name, reassign it to a new function that calls the old one then does its
 * own thing (220-v2.js, 260-cascade.js each did this independently). That
 * only worked because these are classic scripts sharing one global scope and
 * script load order is load-bearing, and it was provably leaky: 10-sse.js
 * special-cased token_usage and returned before ever calling the (possibly
 * wrapped) addEvent, so 220-v2.js's own token-tile updater never ran no
 * matter how it registered itself. plan_updated/workflow_updated had a THIRD
 * routing path — 10-sse.js updated the Blueprint board directly and
 * returned, bypassing addEvent (and therefore the wrapped chain) entirely.
 *
 * Everything now goes through one real subscription API: 10-sse.js emits
 * every SSE event unconditionally, and every module that cares registers its
 * own listener instead of wrapping a global function.
 */

const EventBus = (() => {
  const listeners = {}; // type -> [handler, ...]
  const anyListeners = []; // handlers that see every event, regardless of type

  function on(type, handler) {
    (listeners[type] || (listeners[type] = [])).push(handler);
  }

  // onAny registers a handler that runs for every emitted event, in
  // registration order — the direct replacement for "wrap window.addEvent"
  // (220-v2.js's handleV2Event and 40-events.js's addEvent both use this;
  // since script load order is unchanged, so is call order).
  function onAny(handler) {
    anyListeners.push(handler);
  }

  // emit fans out to type-specific listeners first, then onAny listeners —
  // same relative order the old wrap chain produced (addEvent's own logic
  // ran, then each wrapper's "and also do this"). A handler throwing does not
  // stop the others; it wasn't allowed to before either (10-sse.js already
  // wraps each SSE callback in try/catch).
  function emit(type, data) {
    (listeners[type] || []).forEach((h) => {
      try { h(data); } catch (e) { console.error("EventBus: " + type + " listener failed:", e); }
    });
    anyListeners.forEach((h) => {
      try { h(data); } catch (e) { console.error("EventBus: onAny listener failed for " + type + ":", e); }
    });
  }

  // emitAny fans out to onAny listeners only, skipping type-specific ones —
  // for history replay (170-init.js), which under the old wrap-chain design
  // only ever reached "whatever function currently holds the addEvent name"
  // (the onAny-equivalent consumers: raw feed, exec-status-bar, cascade
  // rungs). It never reached 10-sse.js's live-dispatch-only special-casing
  // (project auto-activation, live plan-board updates) — that logic lived
  // solely inside the SSE callback and history replay never called it. Those
  // side effects are now type-specific EventBus subscribers (100-projects.js
  // etc.), and re-firing them from stale history on every reload — jumping
  // the user to the Blueprint tab because of an old "project detected" event
  // — would be a new, unwanted behavior replay never had. emitAny keeps
  // replay's reach exactly what it always was.
  function emitAny(type, data) {
    anyListeners.forEach((h) => {
      try { h(data); } catch (e) { console.error("EventBus: onAny listener failed for " + type + ":", e); }
    });
  }

  return {
    on,
    onAny,
    emit,
    emitAny,
    // currentActivity is a small shared snapshot of "what's happening right
    // now" — populated in place by whichever view already computes each
    // field (220-v2.js's exec-status-bar), not a second source of truth.
    // Lets a future view (e.g. the Blueprint tab) read current pipeline
    // stage / token-cost state without re-deriving it from raw events.
    currentActivity: {
      stage: null,
      stageState: null,
      tokens: 0,
      cost: 0,
      latencyMs: 0,
      contextTokens: 0,
    },
  };
})();
