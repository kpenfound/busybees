// Package ops supplies operational primitives for caller-owned factory loops:
// retry decisions, accounting and budget signals, capacity and degraded
// episodes, bounded events, fair slots and level-triggered wakes. It assigns
// no meaning to work keys, tags, roles or outcomes. Callers own workflow,
// logging, persistence migration and the consequences of operational signals.
package ops
