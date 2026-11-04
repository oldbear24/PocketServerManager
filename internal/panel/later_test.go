package panel

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestLaterWorkRunsUntilStopped(t *testing.T) {
	var work laterWork
	var ran atomic.Int32

	done := make(chan struct{})
	work.after(time.Millisecond, func() {
		ran.Add(1)
		close(done)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("it did not run")
	}

	// what has not started never does, and what runs is waited for
	work.after(time.Hour, func() { ran.Add(1) })
	started := make(chan struct{})
	release := make(chan struct{})
	work.after(time.Millisecond, func() {
		close(started)
		<-release
		ran.Add(1)
	})
	<-started

	stopped := make(chan struct{})
	go func() {
		work.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop did not wait for the running function")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-stopped

	work.after(time.Millisecond, func() { ran.Add(1) })
	time.Sleep(20 * time.Millisecond)
	if got := ran.Load(); got != 2 {
		t.Errorf("ran %d times", got)
	}
}
