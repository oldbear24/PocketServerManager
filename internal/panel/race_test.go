//go:build race

package panel

// raceEnabled tells timing checks that the race detector slows everything down
// many times over: what they measure then says nothing about the panel.
const raceEnabled = true
