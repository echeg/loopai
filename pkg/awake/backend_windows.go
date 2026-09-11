//go:build windows

package awake

import (
	"fmt"
	"runtime"
	"sync"

	"golang.org/x/sys/windows"
)

const (
	esSystemRequired = 0x00000001
	esContinuous     = 0x80000000
)

// platformBackend returns the SetThreadExecutionState backend. The call is always available on
// Windows, so the backend is never nil.
func platformBackend() Backend { return &threadStateBackend{} }

// threadStateBackend drives SetThreadExecutionState from one locked OS thread. The continuous
// flag is per thread, so acquire and release must run on the same thread, which goroutines do not
// guarantee on their own. The state clears with the process, so a crash releases the hold.
type threadStateBackend struct {
	mu       sync.Mutex
	requests chan bool
	results  chan error
}

func (b *threadStateBackend) Acquire() error { return b.set(true) }

func (b *threadStateBackend) Release() { _ = b.set(false) }

func (b *threadStateBackend) set(hold bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.requests == nil {
		b.requests = make(chan bool)
		b.results = make(chan error)
		go b.serve()
	}
	b.requests <- hold
	return <-b.results
}

func (b *threadStateBackend) serve() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadExecutionState")
	for hold := range b.requests {
		flags := uintptr(esContinuous)
		if hold {
			flags |= esSystemRequired
		}
		r, _, err := proc.Call(flags)
		if r == 0 {
			b.results <- fmt.Errorf("SetThreadExecutionState: %w", err)
			continue
		}
		b.results <- nil
	}
}
