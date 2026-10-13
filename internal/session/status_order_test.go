package session

import (
	"reflect"
	"sync"
	"testing"
)

func TestLiveFirstOrder(t *testing.T) {
	statuses := []Status{StatusError, StatusStopped, StatusRunning, StatusError, StatusIdle, StatusWaiting, StatusStopped, StatusStarting, StatusQueued}
	instances := make([]*Instance, len(statuses))
	for i, s := range statuses {
		instances[i] = &Instance{Status: s}
	}
	got := LiveFirstOrder(instances)
	// Queued stays live: updateStatus probes its pane to catch the start.
	want := []int{2, 4, 5, 7, 8, 0, 1, 3, 6}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LiveFirstOrder = %v, want %v", got, want)
	}
	seen := make(map[int]bool, len(got))
	for _, i := range got {
		if seen[i] {
			t.Fatalf("index %d repeated in %v", i, got)
		}
		seen[i] = true
	}
	if len(seen) != len(instances) {
		t.Fatalf("got %d indices, want %d", len(seen), len(instances))
	}
}

func TestLiveFirstOrderEmpty(t *testing.T) {
	if got := LiveFirstOrder(nil); len(got) != 0 {
		t.Fatalf("LiveFirstOrder(nil) = %v, want empty", got)
	}
}

func TestLiveFirstOrderConcurrentStatusWriter(t *testing.T) {
	instances := []*Instance{{Status: StatusRunning}, {Status: StatusError}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			instances[i%2].SetStatusThreadSafe(StatusStopped)
			instances[i%2].SetStatusThreadSafe(StatusRunning)
		}
	}()
	// The check is -race catching a read of inst.Status that bypasses the lock.
	for i := 0; i < 1000; i++ {
		_ = LiveFirstOrder(instances)
	}
	wg.Wait()
}
