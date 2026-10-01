//go:build race

package hive

// raceEnabled: the race detector changes allocation counts (sync.Pool
// drops items at random under it), so allocation bounds aren't checked.
const raceEnabled = true
