//go:build race

package library

// Allocation counts are not meaningful under -race (sync.Pool drops items, instrumentation allocates).
const raceOn = true
