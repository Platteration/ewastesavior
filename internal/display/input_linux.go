//go:build linux

package display

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// watchInput calls activity whenever any /dev/input/event* device reports
// an event (keyboard, mouse, touchpad), rescanning for hotplugged devices
// every 2 s. Devices are read without grabbing them, so the console still
// gets every key. It returns when ctx ends.
func watchInput(ctx context.Context, dir string, activity func()) {
	var mu sync.Mutex
	open := map[string]*os.File{}
	defer func() {
		mu.Lock()
		for _, f := range open {
			f.Close()
		}
		mu.Unlock()
	}()
	var last time.Time
	var lastMu sync.Mutex
	notify := func() {
		lastMu.Lock()
		defer lastMu.Unlock()
		if time.Since(last) >= time.Second { // at most one wakeup per second
			last = time.Now()
			activity()
		}
	}
	scan := func() {
		names, _ := filepath.Glob(filepath.Join(dir, "event*"))
		for _, name := range names {
			mu.Lock()
			_, ok := open[name]
			mu.Unlock()
			if ok {
				continue
			}
			// Non-blocking so the file joins the runtime poller: no thread
			// per device, and Close interrupts the read.
			f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				continue
			}
			mu.Lock()
			open[name] = f
			mu.Unlock()
			go func() {
				buf := make([]byte, 24*16) // struct input_event is 16 or 24 bytes
				for {
					n, err := f.Read(buf)
					if n > 0 {
						notify()
					}
					if err != nil {
						if errors.Is(err, os.ErrClosed) || ctx.Err() != nil {
							return
						}
						mu.Lock()
						delete(open, name)
						mu.Unlock()
						f.Close()
						return
					}
				}
			}()
		}
	}
	scan()
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scan()
		}
	}
}
