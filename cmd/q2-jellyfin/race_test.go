//go:build race

package main

// Allocation counts are not meaningful under -race (sync.Pool drops items, instrumentation allocates).
const raceOn = true
