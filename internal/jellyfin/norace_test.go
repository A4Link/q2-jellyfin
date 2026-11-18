//go:build !race

package jellyfin

// Allocation counts are not meaningful under -race (sync.Pool drops items, instrumentation allocates).
const raceOn = false
