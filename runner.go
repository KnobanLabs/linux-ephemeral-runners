package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

type runnerState int

const (
	statePending runnerState = iota // waiting for capacity, booting, registering
	stateIdle                       // registered and listening for a job
	stateBusy                       // running a job
	stateDone                       // tearing down; no longer counted
)

func (s runnerState) String() string {
	return [...]string{"pending", "idle", "busy", "done"}[s]
}

// runner is one VM serving exactly one job.
type runner struct {
	name     string
	cancel   context.CancelFunc
	finished chan struct{} // closed once the VM is gone

	mu        sync.Mutex
	state     runnerState
	runnerID  int
	busySince time.Time
	completed chan struct{} // closed when GitHub reports the job finished
	closeOnce sync.Once
}

func (r *runner) getState() runnerState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *runner) setState(s runnerState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == stateDone {
		return
	}
	r.state = s
	if s == stateBusy {
		r.busySince = time.Now()
	}
}

func (r *runner) markCompleted() { r.closeOnce.Do(func() { close(r.completed) }) }

const (
	// listenTimeout covers booting the VM and the runner connecting.
	listenTimeout   = 5 * time.Minute
	timeoutInterval = 30 * time.Second
	failureBackoff  = 15 * time.Second
	cleanupTimeout  = 2 * time.Minute
	// guestWorkFolder matches GitHub-hosted Ubuntu runners, where jobs run
	// in /home/runner/work/<repo>/<repo>; some workflows assume it.
	guestWorkFolder = "/home/runner/work"
)

// runVM takes a runner from capacity to teardown. It never returns until the
// VM is deleted and its registration is gone.
func (c *poolController) runVM(ctx context.Context, r *runner) {
	log := c.log.With(slog.String("runner", r.name))
	defer close(r.finished)

	err := c.serve(ctx, log, r)
	switch {
	case ctx.Err() != nil:
		log.Info("Runner stopped")
	case err != nil:
		log.Error("Runner failed", slog.Any("error", err))
		// Keep counting this runner as pending for a moment so a broken
		// image or GitHub outage doesn't become a tight clone/boot loop.
		// Scaling down can still cancel it.
		r.mu.Lock()
		if r.state != stateDone {
			r.state = statePending
		}
		r.runnerID = 0
		r.mu.Unlock()
		select {
		case <-time.After(failureBackoff):
		case <-ctx.Done():
		}
	}
	r.setState(stateDone)
	c.forget(r)
}

func (c *poolController) serve(ctx context.Context, log *slog.Logger, r *runner) error {
	lease, err := c.host.Acquire(ctx, c.pool.MemoryMB, c.pool.CPU)
	if err != nil {
		return err
	}
	defer c.host.Release(lease)

	// Everything below runs on cleanup even if ctx is canceled.
	cleanupCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	}

	log.Info("Creating VM", slog.String("image", c.pool.Image))
	start := time.Now()
	dir := filepath.Join(c.vmsDir, r.name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create VM directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Error("Failed to delete VM", slog.Any("error", err))
			return
		}
		log.Info("VM deleted")
	}()
	disk := filepath.Join(dir, "disk.qcow2")
	if err := c.qemu.CreateOverlay(ctx, c.image, disk); err != nil {
		return err
	}

	// The runner's single-use registration is made before boot, because
	// the VM starts its runner straight away from it.
	jit, err := c.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name:       r.name,
		WorkFolder: guestWorkFolder,
	}, c.scaleSet.ID)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}
	r.mu.Lock()
	r.runnerID = jit.Runner.ID
	r.mu.Unlock()
	var vm *VM
	defer func() {
		// An ephemeral runner deregisters itself after its job; this
		// catches the ones that never got that far. The VM goes first so
		// GitHub doesn't refuse because a job is still running on it.
		vm.Stop()
		cctx, cancel := cleanupCtx()
		defer cancel()
		err := c.client.RemoveRunner(cctx, int64(jit.Runner.ID))
		if err != nil && !isNotFound(err) {
			log.Warn("Failed to remove runner registration", slog.Any("error", err))
		}
	}()

	// The registration reaches the VM as a tiny read-only disk, which goes
	// with the VM's directory.
	jitFile := filepath.Join(dir, "jit.raw")
	if err := os.WriteFile(jitFile, jitDisk(jit.EncodedJITConfig), 0o600); err != nil {
		return err
	}
	console, err := os.Create(filepath.Join(dir, "console.log"))
	if err != nil {
		return err
	}
	defer console.Close()
	ready := make(chan struct{})
	out := &watchWriter{w: console, needle: []byte("Listening for Jobs"), found: ready}
	vm, err = c.qemu.Start(VMSpec{
		Name:      r.name,
		Disk:      disk,
		CPU:       c.pool.CPU,
		MemoryMB:  c.pool.MemoryMB,
		JITDisk:   jitFile,
		ExtraArgs: c.pool.QEMUArgs,
		PIDFile:   filepath.Join(dir, "qemu.pid"),
	}, out)
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ready:
	case <-vm.Done():
		return fmt.Errorf("VM exited before the runner connected (%v); console:\n%s", vm.Err(), logTail(console.Name()))
	case <-time.After(listenTimeout):
		return fmt.Errorf("runner did not connect within %s; console:\n%s", listenTimeout, logTail(console.Name()))
	}
	r.setState(stateIdle)
	log.Info("Runner ready", slog.Duration("took", time.Since(start).Round(time.Second)))

	timeoutCheck := time.NewTicker(timeoutInterval)
	defer timeoutCheck.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.completed:
			log.Info("Job completed")
			return nil
		case <-vm.Done():
			// The guest powers off once its runner exits after the job.
			log.Info("VM shut down", slog.Any("exit", vm.Err()))
			return nil
		case <-timeoutCheck.C:
			r.mu.Lock()
			since := r.busySince
			r.mu.Unlock()
			if !since.IsZero() && c.jobTimeout > 0 && time.Since(since) > c.jobTimeout {
				return fmt.Errorf("job exceeded job_timeout of %s", c.jobTimeout)
			}
		}
	}
}

// jitDisk lays a registration out as a disk image: one line of text, padded
// with zeros to whole 512-byte sectors. The guest reads the first line.
func jitDisk(jit string) []byte {
	data := []byte(jit + "\n")
	if pad := len(data) % 512; pad != 0 {
		data = append(data, make([]byte, 512-pad)...)
	}
	return data
}

// watchWriter copies to w and closes found the first time needle appears.
type watchWriter struct {
	w      io.Writer
	needle []byte
	found  chan struct{}
	tail   []byte
	seen   bool
}

func (w *watchWriter) Write(p []byte) (int, error) {
	if !w.seen {
		// Keep enough of the previous write to catch a needle split
		// across two writes.
		buf := append(w.tail, p...)
		if bytes.Contains(buf, w.needle) {
			w.seen = true
			close(w.found)
		} else {
			w.tail = append([]byte(nil), buf[max(0, len(buf)-len(w.needle)):]...)
		}
	}
	return w.w.Write(p)
}

func logTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(unavailable)"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	return strings.Join(lines[max(0, len(lines)-20):], "\n")
}
