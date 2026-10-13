package main

import (
	"context"
	"testing"
	"time"
)

func acquireAsync(h *Host, mem, cpu int) <-chan *Lease {
	ch := make(chan *Lease, 1)
	go func() {
		l, _ := h.Acquire(context.Background(), mem, cpu)
		ch <- l
	}()
	return ch
}

func waitQueued(t *testing.T, h *Host, n int) {
	t.Helper()
	for range 100 {
		h.mu.Lock()
		got := len(h.waitQueue)
		h.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d waiters", n)
}

func TestHostFirstComeFirstServed(t *testing.T) {
	h := NewHost(48, 100)
	ctx := context.Background()
	l1, _ := h.Acquire(ctx, 16, 4)
	l2, _ := h.Acquire(ctx, 16, 4)
	_, _ = h.Acquire(ctx, 8, 4)

	big := acquireAsync(h, 16, 4)
	waitQueued(t, h, 1)
	small := acquireAsync(h, 8, 4)
	waitQueued(t, h, 2)

	// 8 MB is free, enough for the small request, but the big one is ahead
	// of it in the queue, so it waits.
	select {
	case <-small:
		t.Fatal("small request jumped ahead of the waiting big one")
	case <-time.After(50 * time.Millisecond):
	}
	h.Release(l1)
	if l := <-big; l == nil {
		t.Fatal("big request should get the freed memory")
	}
	if l := <-small; l == nil {
		t.Fatal("small request never got capacity")
	}
	_ = l2
}

func TestHostCPUBudget(t *testing.T) {
	h := NewHost(100, 8)
	ctx := context.Background()
	l, _ := h.Acquire(ctx, 4, 4)
	_, _ = h.Acquire(ctx, 4, 4)
	third := acquireAsync(h, 4, 4)
	waitQueued(t, h, 1)
	h.Release(l)
	if l := <-third; l == nil {
		t.Fatal("third VM never got CPU")
	}
}

func TestHostAcquireCanceled(t *testing.T) {
	h := NewHost(4, 100)
	_, _ = h.Acquire(context.Background(), 4, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Acquire(ctx, 4, 1); err == nil {
		t.Fatal("expected cancellation")
	}
	if len(h.waitQueue) != 0 {
		t.Fatal("canceled request left in queue")
	}
	// A canceled head of the queue must not block the next request.
	if _, err := h.Acquire(context.Background(), 0, 1); err != nil {
		t.Fatal(err)
	}
}

func TestWatchWriterSplitNeedle(t *testing.T) {
	found := make(chan struct{})
	w := &watchWriter{w: discard{}, needle: []byte("Listening for Jobs"), found: found}
	w.Write([]byte("√ Connected to GitHub\nListening f"))
	w.Write([]byte("or Jobs\n"))
	select {
	case <-found:
	default:
		t.Fatal("needle split across writes was missed")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
