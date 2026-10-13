package main

import (
	"context"
	"slices"
	"sync"
)

// Host hands out this machine's capacity to VMs from every pool: shared
// memory and vCPU budgets.
//
// Requests are granted in arrival order, so a large VM is not starved by a
// stream of small ones.
type Host struct {
	mu        sync.Mutex
	changed   chan struct{}
	memFree   int
	cpuFree   int
	nextSeq   uint64
	waitQueue []*hostRequest
}

type hostRequest struct {
	seq uint64
	mem int
	cpu int
}

// Lease is capacity held by one VM. Release returns it.
type Lease struct {
	mem int
	cpu int
}

func NewHost(memoryMB, vcpus int) *Host {
	return &Host{changed: make(chan struct{}), memFree: memoryMB, cpuFree: vcpus}
}

// Acquire blocks until the request fits, or ctx is done.
func (h *Host) Acquire(ctx context.Context, memMB, cpu int) (*Lease, error) {
	h.mu.Lock()
	req := &hostRequest{seq: h.nextSeq, mem: memMB, cpu: cpu}
	h.nextSeq++
	h.waitQueue = append(h.waitQueue, req)
	h.mu.Unlock()

	for {
		h.mu.Lock()
		if lease, ok := h.tryTake(req); ok {
			h.mu.Unlock()
			return lease, nil
		}
		changed := h.changed
		h.mu.Unlock()

		select {
		case <-ctx.Done():
			h.mu.Lock()
			h.dequeue(req)
			h.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (h *Host) tryTake(req *hostRequest) (*Lease, bool) {
	// An older request is still waiting; it goes first.
	if len(h.waitQueue) > 0 && h.waitQueue[0] != req {
		return nil, false
	}
	if req.mem > h.memFree || req.cpu > h.cpuFree {
		return nil, false
	}
	h.memFree -= req.mem
	h.cpuFree -= req.cpu
	h.dequeue(req)
	return &Lease{mem: req.mem, cpu: req.cpu}, true
}

func (h *Host) dequeue(req *hostRequest) {
	h.waitQueue = slices.DeleteFunc(h.waitQueue, func(r *hostRequest) bool { return r == req })
	h.broadcast()
}

func (h *Host) Release(l *Lease) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.memFree += l.mem
	h.cpuFree += l.cpu
	h.broadcast()
}

// broadcast wakes every waiter. Callers hold h.mu.
func (h *Host) broadcast() {
	close(h.changed)
	h.changed = make(chan struct{})
}
