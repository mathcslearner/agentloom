//go:build race

package dag_test

// raceEnabled reports whether the race detector instruments this build;
// the 10k performance gate is informational-only under -race.
const raceEnabled = true
