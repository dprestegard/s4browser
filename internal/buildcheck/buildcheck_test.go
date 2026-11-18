// Package buildcheck holds a placeholder test that proves the toolchain
// (Go, CGO, and the race detector) compiles and links. It is removed or
// superseded once real engine tests exist in stage 0.3 onward.
package buildcheck

import (
	"sync"
	"testing"
)

func TestRaceDetectorLinks(t *testing.T) {
	var (
		mu    sync.Mutex
		count int
		wg    sync.WaitGroup
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			count++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if count != 8 {
		t.Fatalf("count = %d, want 8", count)
	}
}
