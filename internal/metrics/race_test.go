//go:build race

package metrics

// raceEnabled tells timing checks that the race detector slows everything down
// many times over: what they measure then says nothing about the store.
const raceEnabled = true
