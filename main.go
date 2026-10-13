// Command ephemeral-runners autoscales GitHub Actions runners on a Linux
// host, one fresh QEMU/KVM VM per job, deleted as soon as the job is done.
// One controller serves every deployment (org, repo or enterprise)
// configured on the host, sharing its capacity between them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/actions/scaleset"
)

const usage = `usage: ephemeral-runners [-config-dir dir] <command>

commands:
  run      register every deployment's scale sets and serve jobs until stopped
  check    validate the config, credentials, images, KVM and budget, then exit
  cleanup  delete leftover VMs and their runner registrations, then exit
  build-image [-disk-gb N] [name...]
           build golden images (default: every image the deployments use).
           Names are ubuntu-<version>, e.g. ubuntu-24.04
  remove-scale-sets <deployment.toml>
           delete one deployment's runner scale sets from GitHub, to retire
           it; take it out of the controller first (scripts/remove-deployment.sh)
  validate-config <deployment.toml>...
           check deployment files offline (no GitHub, QEMU or keys needed),
           e.g. in a config repo's CI

The config dir holds host.toml (optional) and deployments/<name>.toml.
`

func main() {
	configDir := flag.String("config-dir", defaultConfigDir, "config directory")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch flag.Arg(0) {
	case "validate-config":
		if flag.NArg() < 2 {
			flag.Usage()
			os.Exit(2)
		}
		if err := validateConfig(flag.Args()[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case "build-image":
		if err := buildImagesCommand(ctx, *configDir, flag.Args()[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	case "remove-scale-sets":
		if flag.NArg() != 2 {
			flag.Usage()
			os.Exit(2)
		}
		log := HostConfig{LogLevel: "info"}.Logger()
		if err := removeScaleSets(ctx, flag.Arg(1), log); err != nil {
			log.Error("Exiting", slog.Any("error", err))
			os.Exit(1)
		}
		return
	}

	cfg, err := LoadConfig(*configDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := cfg.Host.Logger()

	switch flag.Arg(0) {
	case "run":
		err = run(ctx, cfg, log)
	case "check":
		err = check(ctx, cfg, log)
	case "cleanup":
		err = cleanup(ctx, cfg, log)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("Exiting", slog.Any("error", err))
		os.Exit(1)
	}
}

func newClient(ctx context.Context, d *Deployment) (*scaleset.Client, error) {
	info := scaleset.SystemInfo{System: "ephemeral-runners", Subsystem: "qemu", Version: "0.1.0", CommitSHA: "NA"}
	if app := d.GitHubApp; app != nil {
		key, err := os.ReadFile(app.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("deployment %q: read GitHub App private key: %w", d.Name, err)
		}
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: d.RegistrationURL,
			GitHubAppAuth: scaleset.GitHubAppAuth{
				ClientID:       app.ClientID,
				InstallationID: app.InstallationID,
				PrivateKey:     string(key),
			},
			SystemInfo: info,
		})
	}
	out, err := exec.CommandContext(ctx, "sh", "-c", d.TokenCommand).Output()
	if err != nil {
		return nil, fmt.Errorf("deployment %q: token_command failed: %w", d.Name, err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return nil, fmt.Errorf("deployment %q: token_command printed nothing", d.Name)
	}
	return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     d.RegistrationURL,
		PersonalAccessToken: token,
		SystemInfo:          info,
	})
}

func newClients(ctx context.Context, cfg *Config) (map[string]*scaleset.Client, error) {
	clients := map[string]*scaleset.Client{}
	for _, d := range cfg.Deployments {
		c, err := newClient(ctx, d)
		if err != nil {
			return nil, err
		}
		clients[d.Name] = c
	}
	return clients, nil
}

func runnerGroupID(ctx context.Context, client *scaleset.Client, name string) (int, error) {
	if isDefaultGroup(name) {
		return 1, nil
	}
	group, err := client.GetRunnerGroupByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("runner group %q: %w", name, err)
	}
	return group.ID, nil
}

func logBudget(log *slog.Logger, cfg *Config, b HostBudget) {
	names := make([]string, len(cfg.Deployments))
	for i, d := range cfg.Deployments {
		names[i] = d.Name
	}
	log.Info("Host budget", slog.Int("memoryMB", b.MemoryMB), slog.Int("vcpus", b.VCPUs), slog.Any("deployments", names))
}

func run(ctx context.Context, cfg *Config, log *slog.Logger) error {
	qemu, err := newQEMU(cfg.Host.QEMU)
	if err != nil {
		return err
	}
	if err := CheckKVM(); err != nil {
		return err
	}
	clients, err := newClients(ctx, cfg)
	if err != nil {
		return err
	}
	if err := checkImages(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Host.VMsDir(), 0o700); err != nil {
		return err
	}
	if err := cleanupLeftovers(ctx, log, cfg.Host.VMsDir(), clients, cfg.Host.VMPrefix); err != nil {
		return fmt.Errorf("clean up leftover VMs: %w", err)
	}

	budget, err := cfg.ResolveBudget()
	if err != nil {
		return err
	}
	logBudget(log, cfg, budget)
	host := NewHost(budget.MemoryMB, budget.VCPUs)
	// Runners get their own context so shutdown can drain them after the
	// listeners have stopped taking new jobs.
	runnersCtx, stopRunners := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRunners()

	var controllers []*poolController
	for _, d := range cfg.Deployments {
		client := clients[d.Name]
		groupID, err := runnerGroupID(ctx, client, d.RunnerGroup)
		if err != nil {
			return fmt.Errorf("deployment %q: %w", d.Name, err)
		}
		for _, pool := range d.Pools {
			ss, err := ensureScaleSet(ctx, client, groupID, pool)
			if err != nil {
				return fmt.Errorf("deployment %q: %w", d.Name, err)
			}
			log.Info("Scale set ready", slog.String("deployment", d.Name), slog.String("name", ss.Name),
				slog.Int("id", ss.ID), slog.Int("maxRunners", pool.MaxRunners), slog.String("image", pool.Image))
			controllers = append(controllers, &poolController{
				deployment:  d.Name,
				pool:        pool,
				prefix:      cfg.Host.VMPrefix,
				jobTimeout:  d.JobTimeout.Duration,
				owner:       hostname() + "/" + d.Name + "/" + pool.Name,
				image:       cfg.Host.ImagePath(pool.Image),
				vmsDir:      cfg.Host.VMsDir(),
				client:      client,
				scaleSet:    ss,
				host:        host,
				qemu:        qemu,
				log:         log.With(slog.String("deployment", d.Name), slog.String("pool", pool.Name)),
				listenerLog: cfg.Host.ListenerLogger().With(slog.String("deployment", d.Name), slog.String("pool", pool.Name)),
				runnersCtx:  runnersCtx,
				runners:     map[string]*runner{},
			})
		}
	}

	var wg sync.WaitGroup
	for _, c := range controllers {
		wg.Go(func() { c.listen(ctx) })
	}
	<-ctx.Done()
	wg.Wait()

	log.Info("Shutting down", "drainTimeout", cfg.Host.DrainTimeout.Duration)
	for _, c := range controllers {
		wg.Go(func() { c.drain(cfg.Host.DrainTimeout.Duration) })
	}
	wg.Wait()
	log.Info("All runners stopped")
	return nil
}

// checkImages fails fast when a pool's golden image hasn't been built.
func checkImages(cfg *Config) error {
	var errs []error
	for _, name := range cfg.Images() {
		if _, err := os.Stat(cfg.Host.ImagePath(name)); err != nil {
			errs = append(errs, fmt.Errorf("image %s is missing (%w); build it with `ephemeral-runners build-image %s`", name, err, name))
		}
	}
	return errors.Join(errs...)
}

func check(ctx context.Context, cfg *Config, log *slog.Logger) error {
	if _, err := newQEMU(cfg.Host.QEMU); err != nil {
		return err
	}
	if err := CheckKVM(); err != nil {
		return err
	}
	budget, err := cfg.ResolveBudget()
	if err != nil {
		return err
	}
	logBudget(log, cfg, budget)
	if err := checkImages(cfg); err != nil {
		return err
	}
	log.Info("KVM, QEMU and images present", slog.Any("images", cfg.Images()))
	for _, d := range cfg.Deployments {
		client, err := newClient(ctx, d)
		if err != nil {
			return err
		}
		groupID, err := runnerGroupID(ctx, client, d.RunnerGroup)
		if err != nil {
			return fmt.Errorf("deployment %q: %w", d.Name, err)
		}
		for _, p := range d.Pools {
			ss, err := client.GetRunnerScaleSet(ctx, groupID, p.Name)
			if err != nil {
				return fmt.Errorf("deployment %q: GitHub API check failed: %w", d.Name, err)
			}
			state := "will be created on first run"
			if ss != nil {
				state = fmt.Sprintf("exists (id %d)", ss.ID)
			}
			log.Info("Scale set", slog.String("deployment", d.Name), slog.String("name", p.Name), slog.String("state", state))
		}
	}
	log.Info("Config OK")
	return nil
}

func cleanup(ctx context.Context, cfg *Config, log *slog.Logger) error {
	clients, err := newClients(ctx, cfg)
	if err != nil {
		return err
	}
	return cleanupLeftovers(ctx, log, cfg.Host.VMsDir(), clients, cfg.Host.VMPrefix)
}

// buildImagesCommand builds the named images, or with none named, every
// image the installed deployments use.
func buildImagesCommand(ctx context.Context, configDir string, args []string) error {
	fs := flag.NewFlagSet("build-image", flag.ContinueOnError)
	diskGB := fs.Int("disk-gb", 80, "disk size of the image (jobs get this much disk)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	host, err := LoadHost(configDir)
	if err != nil {
		return err
	}
	names := fs.Args()
	if len(names) == 0 {
		cfg, err := LoadConfig(configDir)
		if err != nil {
			return err
		}
		names = cfg.Images()
	}
	log := host.Logger()
	qemu, err := newQEMU(host.QEMU)
	if err != nil {
		return err
	}
	if err := CheckKVM(); err != nil {
		return err
	}
	for _, name := range names {
		if !imageNameRE.MatchString(name) {
			return fmt.Errorf("image %q must be a plain name such as ubuntu-24.04", name)
		}
		if err := buildImage(ctx, log, qemu, host, name, *diskGB); err != nil {
			return fmt.Errorf("image %s: %w", name, err)
		}
	}
	return nil
}

func removeScaleSets(ctx context.Context, path string, log *slog.Logger) error {
	d, err := LoadDeployment(path)
	if err != nil {
		return err
	}
	client, err := newClient(ctx, d)
	if err != nil {
		return err
	}
	groupID, err := runnerGroupID(ctx, client, d.RunnerGroup)
	if err != nil {
		return err
	}
	for _, p := range d.Pools {
		ss, err := client.GetRunnerScaleSet(ctx, groupID, p.Name)
		if err != nil {
			return err
		}
		if ss == nil {
			log.Info("Scale set not found", slog.String("deployment", d.Name), slog.String("name", p.Name))
			continue
		}
		if err := client.DeleteRunnerScaleSet(ctx, ss.ID); err != nil {
			return fmt.Errorf("delete scale set %q: %w", p.Name, err)
		}
		log.Info("Scale set deleted", slog.String("deployment", d.Name), slog.String("name", p.Name), slog.Int("id", ss.ID))
	}
	return nil
}

// validateConfig loads deployment files the way the controller would, and
// checks them against each other, without touching GitHub, QEMU or keys.
func validateConfig(paths []string) error {
	cfg := &Config{Host: defaultHost()}
	var errs []error
	for _, p := range paths {
		d, err := LoadDeployment(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cfg.Deployments = append(cfg.Deployments, d)
	}
	if len(errs) == 0 {
		errs = append(errs, cfg.Validate())
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	for _, d := range cfg.Deployments {
		fmt.Printf("%s: OK (%d pools)\n", d.Path, len(d.Pools))
	}
	return nil
}
