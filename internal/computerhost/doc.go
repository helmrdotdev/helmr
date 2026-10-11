// Package computerhost owns worker-side Computer allocations, preparation,
// Session transports, live saves, commands and native continuation controls.
// Allocation owners retain physical resources through cleanup and durable stop
// acknowledgment; shared artifact and device helpers do not own execution.
package computerhost
