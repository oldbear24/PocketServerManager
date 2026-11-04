package panel

import (
	"sync"
	"time"
)

// laterWork runs functions after a delay while the panel runs. Once it stops,
// nothing more starts, and stopping waits for what is running: the app's
// database closes right after.
type laterWork struct {
	mu      sync.Mutex
	stopped bool
	timers  map[*time.Timer]struct{}
	running sync.WaitGroup
}

// after runs fn once delay has passed, unless the panel stops first.
func (w *laterWork) after(delay time.Duration, fn func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	if w.timers == nil {
		w.timers = map[*time.Timer]struct{}{}
	}

	var timer *time.Timer
	// the timer is set before its function can take the lock
	timer = time.AfterFunc(delay, func() {
		w.mu.Lock()
		delete(w.timers, timer)
		if w.stopped {
			w.mu.Unlock()
			return
		}
		w.running.Add(1)
		w.mu.Unlock()

		defer w.running.Done()
		fn()
	})
	w.timers[timer] = struct{}{}
}

// stop cancels what has not started and waits for what has.
func (w *laterWork) stop() {
	w.mu.Lock()
	w.stopped = true
	for timer := range w.timers {
		timer.Stop()
	}
	w.timers = nil
	w.mu.Unlock()

	w.running.Wait()
}
