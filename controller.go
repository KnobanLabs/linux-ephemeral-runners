package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// poolController keeps one scale set's runners matched to its job demand.
type poolController struct {
	deployment string
	pool       Pool
	prefix     string
	jobTimeout time.Duration
	owner      string
	// image is the pool's golden image; vmsDir holds each VM's directory.
	image  string
	vmsDir string

	client   *scaleset.Client
	scaleSet *scaleset.RunnerScaleSet
	host     *Host
	qemu     *QEMU
	log      *slog.Logger
	// listenerLog is for the scaleset listener's per-poll chatter.
	listenerLog *slog.Logger

	// runnersCtx outlives listener reconnects; canceling it destroys VMs.
	runnersCtx context.Context

	mu      sync.Mutex
	runners map[string]*runner
	wg      sync.WaitGroup
}

var _ listener.Scaler = (*poolController)(nil)

// ensureScaleSet finds the pool's scale set or creates it, and keeps its
// labels in sync with the config.
func ensureScaleSet(ctx context.Context, client *scaleset.Client, groupID int, pool Pool) (*scaleset.RunnerScaleSet, error) {
	labels := []scaleset.Label{{Name: pool.Name}}
	for _, l := range pool.Labels {
		if l != pool.Name {
			labels = append(labels, scaleset.Label{Name: l})
		}
	}
	want := &scaleset.RunnerScaleSet{
		Name:          pool.Name,
		RunnerGroupID: groupID,
		Labels:        labels,
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}

	existing, err := client.GetRunnerScaleSet(ctx, groupID, pool.Name)
	if err != nil {
		return nil, fmt.Errorf("look up scale set %q: %w", pool.Name, err)
	}
	if existing == nil {
		return client.CreateRunnerScaleSet(ctx, want)
	}
	if !sameLabels(existing.Labels, labels) {
		return client.UpdateRunnerScaleSet(ctx, existing.ID, want)
	}
	return existing, nil
}

func sameLabels(a, b []scaleset.Label) bool {
	names := func(ls []scaleset.Label) []string {
		out := make([]string, len(ls))
		for i, l := range ls {
			out[i] = l.Name
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(names(a), names(b))
}

// listen polls the scale set's message queue until ctx is done, opening a
// new session whenever the old one breaks (network loss, sleep, restarts).
func (c *poolController) listen(ctx context.Context) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 2*time.Minute {
			backoff = 5 * time.Second
		}
		c.log.Warn("Listener stopped; reconnecting", slog.Any("error", err), slog.Duration("in", backoff))
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (c *poolController) listenOnce(ctx context.Context) error {
	// A session left by a crashed run blocks new ones until GitHub expires
	// it, so creation errors are retried by the caller's backoff.
	session, err := c.client.MessageSessionClient(ctx, c.scaleSet.ID, c.owner)
	if err != nil {
		return fmt.Errorf("create message session: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = session.Close(cctx)
	}()

	l, err := listener.New(session, listener.Config{
		ScaleSetID: c.scaleSet.ID,
		MaxRunners: c.pool.MaxRunners,
		Logger:     c.listenerLog.With(slog.String("pool", c.pool.Name)),
	})
	if err != nil {
		return err
	}
	c.log.Info("Listening for jobs", slog.Int("scaleSetID", c.scaleSet.ID))
	return l.Run(ctx, c)
}

// The listener acquires jobs and calls these handlers one at a time, ending
// each batch (and each idle long poll) with HandleDesiredRunnerCount.

func (c *poolController) HandleJobStarted(ctx context.Context, j *scaleset.JobStarted) error {
	c.log.Info("Job started", slog.String("runner", j.RunnerName),
		slog.String("repo", j.OwnerName+"/"+j.RepositoryName), slog.String("job", j.JobDisplayName))
	if r := c.lookup(j.RunnerName); r != nil {
		r.setState(stateBusy)
	}
	return nil
}

func (c *poolController) HandleJobCompleted(ctx context.Context, j *scaleset.JobCompleted) error {
	c.log.Info("Job finished", slog.String("runner", j.RunnerName), slog.String("result", j.Result))
	if r := c.lookup(j.RunnerName); r != nil {
		// Stop counting it now, not once its VM is gone, so the next
		// reconcile doesn't see a phantom surplus.
		r.setState(stateDone)
		r.markCompleted()
	}
	return nil
}

func (c *poolController) HandleDesiredRunnerCount(ctx context.Context, assigned int) (int, error) {
	desired := min(c.pool.MaxRunners, c.pool.MinIdle+assigned)
	c.reconcile(ctx, desired)
	return desired, nil
}

// reconcile starts or retires runners so their count matches demand.
func (c *poolController) reconcile(ctx context.Context, desired int) {
	c.mu.Lock()
	var pending, idle []*runner
	active := 0
	for _, r := range c.runners {
		switch r.getState() {
		case statePending:
			pending = append(pending, r)
		case stateIdle:
			idle = append(idle, r)
		case stateDone:
			continue
		}
		active++
	}
	c.mu.Unlock()

	switch {
	case desired > active:
		c.log.Info("Scaling up", slog.Int("active", active), slog.Int("desired", desired))
		for range desired - active {
			c.spawn()
		}
	case desired < active:
		surplus := active - desired
		// Runners that haven't registered yet can go without asking GitHub.
		for _, r := range pending {
			if surplus == 0 {
				return
			}
			r.mu.Lock()
			registered := r.runnerID != 0
			r.mu.Unlock()
			if registered {
				idle = append(idle, r)
				continue
			}
			c.log.Info("Scaling down", slog.String("runner", r.name), slog.String("state", "pending"))
			r.setState(stateDone)
			r.cancel()
			surplus--
		}
		// GitHub refuses to remove a runner it has just handed a job, which
		// is exactly the guard needed here.
		for _, r := range idle {
			if surplus == 0 {
				return
			}
			r.mu.Lock()
			id := r.runnerID
			r.mu.Unlock()
			if err := c.client.RemoveRunner(ctx, int64(id)); err != nil {
				c.log.Debug("Runner not removable; keeping it", slog.String("runner", r.name), slog.Any("error", err))
				continue
			}
			c.log.Info("Scaling down", slog.String("runner", r.name), slog.String("state", "idle"))
			r.setState(stateDone)
			r.cancel()
			surplus--
		}
	}
}

func (c *poolController) spawn() {
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	name := vmName(c.prefix, c.deployment, c.pool.Name, hex.EncodeToString(suffix))

	ctx, cancel := context.WithCancel(c.runnersCtx)
	r := &runner{
		name:      name,
		cancel:    cancel,
		finished:  make(chan struct{}),
		completed: make(chan struct{}),
	}
	c.mu.Lock()
	c.runners[name] = r
	c.mu.Unlock()

	c.wg.Go(func() {
		defer cancel()
		c.runVM(ctx, r)
	})
}

func (c *poolController) lookup(name string) *runner {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runners[name]
}

func (c *poolController) forget(r *runner) {
	c.mu.Lock()
	delete(c.runners, r.name)
	c.mu.Unlock()
}

// drain stops runners that aren't working on a job right away, gives busy
// ones up to timeout to finish, then destroys whatever is left.
func (c *poolController) drain(timeout time.Duration) {
	c.mu.Lock()
	runners := make([]*runner, 0, len(c.runners))
	for _, r := range c.runners {
		runners = append(runners, r)
	}
	c.mu.Unlock()

	deadline := time.After(timeout)
	for _, r := range runners {
		if r.getState() != stateBusy {
			r.cancel()
		}
	}
	for _, r := range runners {
		select {
		case <-r.finished:
		case <-deadline:
			c.log.Warn("Drain timeout reached; destroying busy runners")
			for _, r := range runners {
				r.cancel()
			}
			c.wg.Wait()
			return
		}
	}
	c.wg.Wait()
}

// vmName is the VM's, its directory's and its GitHub runner's name:
// <prefix>-<deployment>-<pool>-<random>. Deployment names have no dashes, so
// the deployment can be read back from a leftover VM's name.
func vmName(prefix, deployment, pool, suffix string) string {
	return fmt.Sprintf("%s-%s-%s-%s", prefix, deployment, pool, suffix)
}

// vmDeployment returns the deployment a VM name belongs to, and whether the
// name has this controller's prefix at all.
func vmDeployment(prefix, name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, prefix+"-")
	if !ok {
		return "", false
	}
	deployment, _, _ := strings.Cut(rest, "-")
	return deployment, true
}

// cleanupLeftovers deletes VMs left behind by a previous run (a crash, a
// power loss) along with their runner registrations. Every VM directory with
// this controller's prefix is deleted, including ones from deployments that
// have since been removed; their registrations are left for GitHub to expire.
func cleanupLeftovers(ctx context.Context, log *slog.Logger, vmsDir string, clients map[string]*scaleset.Client, prefix string) error {
	entries, err := os.ReadDir(vmsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		deployment, ours := vmDeployment(prefix, name)
		if !ours || !e.IsDir() {
			continue
		}
		log.Info("Removing leftover VM", slog.String("vm", name))
		dir := filepath.Join(vmsDir, name)
		killLeftover(filepath.Join(dir, "qemu.pid"), name)
		if err := os.RemoveAll(dir); err != nil {
			log.Error("Failed to delete leftover VM", slog.String("vm", name), slog.Any("error", err))
		}
		client := clients[deployment]
		if client == nil {
			continue
		}
		if ref, err := client.GetRunnerByName(ctx, name); err == nil && ref != nil {
			if err := client.RemoveRunner(ctx, int64(ref.ID)); err != nil {
				log.Warn("Failed to remove leftover runner", slog.String("runner", name), slog.Any("error", err))
			}
		} else if err != nil && !isNotFound(err) {
			log.Warn("Failed to look up leftover runner", slog.String("runner", name), slog.Any("error", err))
		}
	}
	return nil
}
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "ephemeral-runners"
	}
	return h
}

// isNotFound reports whether a scaleset API error means the runner is
// already gone. Some 404s come back without a typed exception body.
func isNotFound(err error) bool {
	return errors.Is(err, scaleset.RunnerNotFoundError) || strings.Contains(err.Error(), `status="404`)
}
