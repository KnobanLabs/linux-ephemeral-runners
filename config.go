package main

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/actions/scaleset"
)

// Config is everything one controller serves, loaded from a directory:
//
//	host.toml                this machine's settings (optional)
//	deployments/<name>.toml  one per org, repo or enterprise served
//
// Deployments share the host's capacity.
type Config struct {
	Dir         string
	Host        HostConfig
	Deployments []*Deployment
}

// HostConfig is this machine's settings, shared by every deployment.
type HostConfig struct {
	// MemoryBudgetMB is the total memory all VMs together may be given.
	// 0 means the host's RAM minus HostReservedMB.
	MemoryBudgetMB int `toml:"memory_budget_mb"`
	// HostReservedMB is memory kept back for the host when the budget is
	// derived from its RAM.
	HostReservedMB int `toml:"host_reserved_mb"`
	// VCPUBudget is the total vCPUs all VMs together may be given. 0 means
	// twice the host's physical cores: VMs rarely keep every vCPU busy,
	// but beyond that jobs slow each other down more than they gain.
	VCPUBudget int `toml:"vcpu_budget"`

	// StateDir holds golden images (images/<name>.qcow2) and each running
	// VM's overlay disk (vms/<vm>/).
	StateDir string `toml:"state_dir"`
	// QEMU is the qemu-system-x86_64 binary.
	QEMU string `toml:"qemu"`

	// VMPrefix marks VMs owned by this controller. Any VM directory under
	// state_dir/vms named "<prefix>-*" is deleted on startup.
	VMPrefix string `toml:"vm_prefix"`
	// DrainTimeout is how long shutdown waits for busy runners to finish
	// before their VMs are destroyed.
	DrainTimeout Duration `toml:"drain_timeout"`
	LogLevel     string   `toml:"log_level"`
}

// Deployment is one org, repo or enterprise this machine runs jobs for.
type Deployment struct {
	// Name comes from the file name. It appears in VM names, so it is
	// limited to lowercase letters and digits.
	Name string `toml:"-"`
	Path string `toml:"-"`

	// RegistrationURL is the org, repo or enterprise the scale sets
	// register with, e.g. https://github.com/my-org.
	RegistrationURL string `toml:"registration_url"`
	// RunnerGroup is the org runner group the scale sets belong to.
	// "Default" always means the org's own default group, not one an
	// enterprise passes down under the same name.
	RunnerGroup string `toml:"runner_group"`

	// TokenCommand prints a GitHub token on stdout. It is run with sh -c,
	// so the token never has to be written into a file.
	TokenCommand string     `toml:"token_command"`
	GitHubApp    *AppConfig `toml:"github_app"`

	// JobTimeout bounds how long a VM may stay busy with one job.
	JobTimeout Duration `toml:"job_timeout"`

	Pools []Pool `toml:"pool"`
}

// AppConfig authenticates as a GitHub App instead of with a token.
type AppConfig struct {
	ClientID       string `toml:"client_id"`
	InstallationID int64  `toml:"installation_id"`
	PrivateKeyPath string `toml:"private_key_path"`
}

// Pool is one runner scale set backed by one golden image.
type Pool struct {
	// Name is the scale set name, and what workflows put in runs-on.
	Name   string   `toml:"name"`
	Labels []string `toml:"labels"`
	// Image names a golden image: state_dir/images/<image>.qcow2. Images
	// named ubuntu-<version> are built by `ephemeral-runners build-image`.
	Image    string `toml:"image"`
	CPU      int    `toml:"cpu"`
	MemoryMB int    `toml:"memory_mb"`
	// MaxRunners is an upper bound; the host budget usually limits
	// concurrency first.
	MaxRunners int `toml:"max_runners"`
	// MinIdle keeps this many booted runners waiting for jobs. Each one
	// holds its memory and CPU while idle.
	MinIdle int `toml:"min_idle"`
	// QEMUArgs are extra arguments for qemu-system-x86_64.
	QEMUArgs []string `toml:"qemu_args"`
}

// Duration lets TOML values like "6h" decode into a time.Duration.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

var (
	deploymentNameRE = regexp.MustCompile(`^[a-z0-9]+$`)
	imageNameRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

const defaultConfigDir = "/etc/ephemeral-runners"

func defaultHost() HostConfig {
	return HostConfig{
		HostReservedMB: 8 * 1024,
		StateDir:       "/var/lib/ephemeral-runners",
		QEMU:           "qemu-system-x86_64",
		VMPrefix:       "eph",
		LogLevel:       "info",
	}
}

// LoadHost reads just host.toml under dir, for commands that don't need
// any deployment.
func LoadHost(dir string) (HostConfig, error) {
	h := defaultHost()
	path := filepath.Join(dir, "host.toml")
	if _, err := toml.DecodeFile(path, &h); err != nil && !errors.Is(err, os.ErrNotExist) {
		return h, fmt.Errorf("read %s: %w", path, err)
	}
	return h, (&Config{Host: h}).Validate()
}

// LoadConfig reads host.toml and every deployments/*.toml under dir.
func LoadConfig(dir string) (*Config, error) {
	host, err := LoadHost(dir)
	if err != nil {
		return nil, err
	}
	cfg := &Config{Dir: dir, Host: host}

	paths, err := filepath.Glob(filepath.Join(dir, "deployments", "*.toml"))
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	var errs []error
	for _, path := range paths {
		d, err := LoadDeployment(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		cfg.Deployments = append(cfg.Deployments, d)
	}
	if len(paths) == 0 {
		errs = append(errs, fmt.Errorf("no deployments in %s", filepath.Join(dir, "deployments")))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, cfg.Validate()
}

// DeploymentName derives a deployment's name from its file: the file name
// without .toml, or for <name>/config.toml, <name>.
func DeploymentName(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".toml")
	if base == "config" {
		return filepath.Base(filepath.Dir(path))
	}
	return base
}

func LoadDeployment(path string) (*Deployment, error) {
	d := &Deployment{
		Name:        DeploymentName(path),
		Path:        path,
		RunnerGroup: "Default",
		JobTimeout:  Duration{6 * time.Hour},
	}
	if _, err := toml.DecodeFile(path, d); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if app := d.GitHubApp; app != nil && strings.HasPrefix(app.PrivateKeyPath, "~/") {
		home, _ := os.UserHomeDir()
		app.PrivateKeyPath = filepath.Join(home, app.PrivateKeyPath[2:])
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("deployment %q (%s): %w", d.Name, path, err)
	}
	return d, nil
}

func (c *Config) Validate() error {
	h := c.Host
	var errs []error
	if h.MemoryBudgetMB < 0 || h.VCPUBudget < 0 || h.HostReservedMB < 0 {
		errs = append(errs, errors.New("memory_budget_mb, vcpu_budget and host_reserved_mb must not be negative"))
	}
	if strings.ContainsAny(h.VMPrefix, " -/") || h.VMPrefix == "" {
		errs = append(errs, errors.New("vm_prefix must be non-empty and contain no spaces, dashes or slashes"))
	}
	if !filepath.IsAbs(h.StateDir) {
		errs = append(errs, errors.New("state_dir must be an absolute path"))
	}
	// Two deployments registering the same scale set would fight over its
	// message session.
	owner := map[string]string{}
	for _, d := range c.Deployments {
		for _, p := range d.Pools {
			key := strings.ToLower(strings.TrimSuffix(d.RegistrationURL, "/")) + " " + p.Name
			if other, ok := owner[key]; ok {
				errs = append(errs, fmt.Errorf("deployments %q and %q both define pool %q for %s", other, d.Name, p.Name, d.RegistrationURL))
			}
			owner[key] = d.Name
		}
	}
	return errors.Join(errs...)
}

func (d *Deployment) Validate() error {
	var errs []error
	if !deploymentNameRE.MatchString(d.Name) {
		errs = append(errs, fmt.Errorf("deployment name %q must be lowercase letters and digits only (it comes from the file name)", d.Name))
	}
	if d.RegistrationURL == "" {
		errs = append(errs, errors.New("registration_url is required"))
	}
	if strings.Contains(d.RegistrationURL, "YOUR-") {
		errs = append(errs, errors.New("registration_url still has a YOUR-* placeholder"))
	}
	if d.TokenCommand == "" && d.GitHubApp == nil {
		errs = append(errs, errors.New("one of token_command or [github_app] is required"))
	}
	if app := d.GitHubApp; app != nil {
		if app.ClientID == "" || strings.Contains(app.ClientID, "YOUR-") || app.InstallationID <= 0 || app.PrivateKeyPath == "" {
			errs = append(errs, errors.New("[github_app] needs client_id, installation_id and private_key_path"))
		}
	}
	if len(d.Pools) == 0 {
		errs = append(errs, errors.New("at least one [[pool]] is required"))
	}
	seen := map[string]bool{}
	for _, p := range d.Pools {
		if p.Name == "" || p.Image == "" {
			errs = append(errs, fmt.Errorf("pool %q: name and image are required", p.Name))
		}
		if p.Image != "" && !imageNameRE.MatchString(p.Image) {
			errs = append(errs, fmt.Errorf("pool %q: image %q must be a plain name such as ubuntu-24.04", p.Name, p.Image))
		}
		if seen[p.Name] {
			errs = append(errs, fmt.Errorf("pool %q: duplicate name", p.Name))
		}
		seen[p.Name] = true
		if p.CPU <= 0 || p.MemoryMB <= 0 || p.MaxRunners <= 0 {
			errs = append(errs, fmt.Errorf("pool %q: cpu, memory_mb and max_runners must be positive", p.Name))
		}
		if p.MinIdle < 0 || p.MinIdle > p.MaxRunners {
			errs = append(errs, fmt.Errorf("pool %q: min_idle must be between 0 and max_runners", p.Name))
		}
	}
	return errors.Join(errs...)
}

// ImagePath is where a golden image lives.
func (h HostConfig) ImagePath(name string) string {
	return filepath.Join(h.StateDir, "images", name+".qcow2")
}

// VMsDir holds one directory per running VM.
func (h HostConfig) VMsDir() string { return filepath.Join(h.StateDir, "vms") }

// Images lists every golden image the deployments use, without repeats.
func (c *Config) Images() []string {
	var names []string
	for _, d := range c.Deployments {
		for _, p := range d.Pools {
			if !slices.Contains(names, p.Image) {
				names = append(names, p.Image)
			}
		}
	}
	slices.Sort(names)
	return names
}

// ListenerLogger is for the scaleset listener, which logs every long poll.
// It stays quiet unless log_level is debug.
func (h HostConfig) ListenerLogger() *slog.Logger {
	if strings.EqualFold(h.LogLevel, "debug") {
		return h.Logger()
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func (h HostConfig) Logger() *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(h.LogLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

// isDefaultGroup reports whether name refers to the org's own default runner
// group, which always has ID 1.
func isDefaultGroup(name string) bool {
	return strings.EqualFold(name, scaleset.DefaultRunnerGroup)
}

// HostBudget is the capacity VMs may use on this machine.
type HostBudget struct {
	MemoryMB int
	VCPUs    int
}

// ResolveBudget fills in budgets left at 0 from this machine's hardware,
// and checks that every pool's VM fits.
func (c *Config) ResolveBudget() (HostBudget, error) {
	b := HostBudget{MemoryMB: c.Host.MemoryBudgetMB, VCPUs: c.Host.VCPUBudget}
	if b.MemoryMB == 0 {
		totalMB, err := memTotalMB("/proc/meminfo")
		if err != nil {
			return b, err
		}
		b.MemoryMB = totalMB - c.Host.HostReservedMB
	}
	if b.VCPUs == 0 {
		cores, err := physicalCores("/proc/cpuinfo")
		if err != nil {
			return b, err
		}
		b.VCPUs = cores * 2
	}
	var errs []error
	for _, d := range c.Deployments {
		for _, p := range d.Pools {
			if p.MemoryMB > b.MemoryMB || p.CPU > b.VCPUs {
				errs = append(errs, fmt.Errorf("%s/%s: one VM (%d MB, %d vCPU) doesn't fit the budget (%d MB, %d vCPU)",
					d.Name, p.Name, p.MemoryMB, p.CPU, b.MemoryMB, b.VCPUs))
			}
		}
	}
	return b, errors.Join(errs...)
}

func memTotalMB(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0, fmt.Errorf("parse MemTotal: %w", err)
			}
			return kb / 1024, nil
		}
	}
	return 0, fmt.Errorf("no MemTotal in %s", path)
}

// physicalCores counts distinct (physical id, core id) pairs, so SMT
// siblings count once.
func physicalCores(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cores := map[string]bool{}
	var physical, core string
	processors := 0
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, ok := strings.Cut(s.Text(), ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "processor":
			processors++
			physical, core = "", ""
		case "physical id":
			physical = strings.TrimSpace(value)
		case "core id":
			core = strings.TrimSpace(value)
			cores[physical+"/"+core] = true
		}
	}
	if len(cores) > 0 {
		return len(cores), nil
	}
	if processors > 0 {
		// Some virtual CPUs report no topology; count threads instead.
		return processors, nil
	}
	return 0, fmt.Errorf("no processors in %s", path)
}
