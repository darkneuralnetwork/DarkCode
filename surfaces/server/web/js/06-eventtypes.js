/* 06-eventtypes.js — the event-type registry, fetched once from the server.
 *
 * core.EventTypes (infra/core/event_meta.go) is the one table mapping an
 * event type to its icon/label/significance; the CLI reads it directly
 * in-process (surfaces/cli/dashboard.go), and /api/event-types is how the
 * browser reads the same table. Before this, 10-sse.js kept its own
 * hardcoded list of every event type name (just to know what to
 * addEventListener for) and 40-events.js kept a separate hardcoded set of
 * which types auto-expand in the raw feed — two more copies of a list that
 * only needed to exist once.
 *
 * Kicked off at file-load time (not on first use, unlike 75-verbs.js's lazy
 * loadVerbs) because connectSSE() needs the full type list before it
 * registers SSE listeners — a type missing from that registration is
 * silently dropped by the browser. Starting the fetch here, as early in the
 * script load order as this file's "06" prefix allows, means the promise is
 * almost always already resolved by the time connectSSE() runs later in
 * init() (which first awaits loadComponents(), a heavier fetch).
 */

const eventTypesPromise = (async () => {
  try {
    const res = await fetch(API + "/api/event-types");
    const data = await res.json();
    return Array.isArray(data.event_types) ? data.event_types : [];
  } catch {
    return []; // connectSSE() falls back to registering nothing extra; SSE reconnects on error anyway
  }
})();

// getEventTypes resolves to the full registry (array of {type, icon, label, significant}).
function getEventTypes() {
  return eventTypesPromise;
}

// significantEventTypes resolves to the Set of type names that should show
// expanded by default in the raw event feed (40-events.js's EVT_EXPANDED_TYPES).
async function significantEventTypes() {
  const types = await getEventTypes();
  return new Set(types.filter((t) => t.significant).map((t) => t.type));
}
