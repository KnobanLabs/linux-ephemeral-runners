package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testPools = `
[[pool]]
name = "ubuntu-x64"
image = "ubuntu-24.04"
cpu = 4
memory_mb = 16384
max_runners = 4

[[pool]]
name = "ubuntu-26.04-x64"
image = "ubuntu-26.04"
cpu = 2
memory_mb = 8192
max_runners = 8
`

func deploymentTOML(url string) string {
	return `registration_url = "` + url + `"
[github_app]
client_id = "Iv1.test"
installation_id = 1
private_key_path = "~/key.pem"
` + testPools
}

func writeConfigDir(t *testing.T, host string, deployments map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "deployments"), 0o700); err != nil {
		t.Fatal(err)
	}
	if host != "" {
		if err := os.WriteFile(filepath.Join(dir, "host.toml"), []byte(host), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range deployments {
		if err := os.WriteFile(filepath.Join(dir, "deployments", name+".toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadConfigMultipleDeployments(t *testing.T) {
	dir := writeConfigDir(t, `vm_prefix = "ci"
memory_budget_mb = 49152
vcpu_budget = 8
state_dir = "/srv/runners"
`, map[string]string{
		"acme":   deploymentTOML("https://github.com/acme"),
		"globex": deploymentTOML("https://github.com/globex"),
	})
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Deployments) != 2 || cfg.Deployments[0].Name != "acme" || cfg.Deployments[1].Name != "globex" {
		t.Fatalf("deployments = %+v", cfg.Deployments)
	}
	if cfg.Host.VMPrefix != "ci" || cfg.Host.QEMU != "qemu-system-x86_64" {
		t.Fatalf("host = %+v", cfg.Host)
	}
	home, _ := os.UserHomeDir()
	if got := cfg.Deployments[0].GitHubApp.PrivateKeyPath; got != filepath.Join(home, "key.pem") {
		t.Fatalf("key path not expanded: %s", got)
	}
	if got := cfg.Host.ImagePath("ubuntu-24.04"); got != "/srv/runners/images/ubuntu-24.04.qcow2" {
		t.Fatalf("ImagePath = %s", got)
	}
	if got := cfg.Images(); !slices.Equal(got, []string{"ubuntu-24.04", "ubuntu-26.04"}) {
		t.Fatalf("Images = %v", got)
	}
	b, err := cfg.ResolveBudget()
	if err != nil || b.MemoryMB != 49152 || b.VCPUs != 8 {
		t.Fatalf("budget = %+v, %v", b, err)
	}
}

func TestLoadConfigRejectsSameScaleSetTwice(t *testing.T) {
	dir := writeConfigDir(t, "", map[string]string{
		"one": deploymentTOML("https://github.com/acme"),
		"two": deploymentTOML("https://github.com/Acme/"),
	})
	_, err := LoadConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "both define pool") {
		t.Fatalf("expected duplicate scale set error, got %v", err)
	}
}

func TestLoadConfigRejectsBadNames(t *testing.T) {
	dir := writeConfigDir(t, "", map[string]string{"acme-corp": deploymentTOML("https://github.com/acme")})
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "lowercase letters and digits") {
		t.Fatalf("expected deployment name error, got %v", err)
	}
	// An image name must not be able to point outside the images directory.
	bad := strings.Replace(deploymentTOML("https://github.com/acme"), `"ubuntu-24.04"`, `"../../etc/x"`, 1)
	dir = writeConfigDir(t, "", map[string]string{"acme": bad})
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "plain name") {
		t.Fatalf("expected image name error, got %v", err)
	}
}

func TestBudgetMustFitEveryPool(t *testing.T) {
	dir := writeConfigDir(t, "memory_budget_mb = 12000\nvcpu_budget = 16\n", map[string]string{
		"acme": deploymentTOML("https://github.com/acme"),
	})
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.ResolveBudget(); err == nil || !strings.Contains(err.Error(), "acme/ubuntu-x64") {
		t.Fatalf("expected the 16 GB pool not to fit, got %v", err)
	}
}

func TestHostHardwareFromProc(t *testing.T) {
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	os.WriteFile(meminfo, []byte("MemTotal:       131072000 kB\nMemFree:        1 kB\n"), 0o600)
	if mb, err := memTotalMB(meminfo); err != nil || mb != 128000 {
		t.Fatalf("memTotalMB = %d, %v", mb, err)
	}
	// Two cores with two threads each: four processors, two cores.
	var cpuinfo strings.Builder
	for i, core := range []string{"0", "1", "0", "1"} {
		cpuinfo.WriteString("processor\t: " + string(rune('0'+i)) + "\nphysical id\t: 0\ncore id\t\t: " + core + "\n\n")
	}
	path := filepath.Join(dir, "cpuinfo")
	os.WriteFile(path, []byte(cpuinfo.String()), 0o600)
	if n, err := physicalCores(path); err != nil || n != 2 {
		t.Fatalf("physicalCores = %d, %v", n, err)
	}
}

func TestRepoConfigs(t *testing.T) {
	if _, err := LoadDeployment("config.example.toml"); err == nil || !strings.Contains(err.Error(), "YOUR-") {
		t.Fatalf("example config should be rejected until filled in, got %v", err)
	}
	host, err := os.ReadFile("host.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	dir := writeConfigDir(t, string(host), map[string]string{"acme": deploymentTOML("https://github.com/acme")})
	if _, err := LoadConfig(dir); err != nil {
		t.Errorf("host.example.toml: %v", err)
	}
}

func TestValidateConfig(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "acme", "config.toml")
	b := filepath.Join(dir, "globex.toml")
	os.MkdirAll(filepath.Dir(a), 0o700)
	os.WriteFile(a, []byte(deploymentTOML("https://github.com/acme")), 0o600)
	os.WriteFile(b, []byte(deploymentTOML("https://github.com/globex")), 0o600)
	if err := validateConfig([]string{a, b}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(b, []byte(deploymentTOML("https://github.com/acme")), 0o600)
	if err := validateConfig([]string{a, b}); err == nil || !strings.Contains(err.Error(), "both define pool") {
		t.Fatalf("expected duplicate scale set error, got %v", err)
	}
}

func TestDeploymentName(t *testing.T) {
	for path, want := range map[string]string{
		"deployments/acme/config.toml":                 "acme",
		"/etc/ephemeral-runners/deployments/acme.toml": "acme",
	} {
		if got := DeploymentName(path); got != want {
			t.Errorf("DeploymentName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestVMNameRoundTrip(t *testing.T) {
	name := vmName("eph", "acme", "ubuntu-26.04-x64", "1a2b3c4d")
	if name != "eph-acme-ubuntu-26.04-x64-1a2b3c4d" {
		t.Fatalf("vmName = %s", name)
	}
	if d, ok := vmDeployment("eph", name); !ok || d != "acme" {
		t.Fatalf("vmDeployment = %q, %v", d, ok)
	}
	for _, other := range []string{"ephx-acme-mac-1", "my-vm", "ephbuild-ubuntu-2404"} {
		if _, ok := vmDeployment("eph", other); ok {
			t.Errorf("%q should not belong to this controller", other)
		}
	}
}

func TestVMSpecArgs(t *testing.T) {
	args := strings.Join(VMSpec{
		Name: "eph-acme-x-1", Disk: "/v/disk.qcow2", CPU: 4, MemoryMB: 16384,
		JITDisk: "/v/jit.raw", PIDFile: "/v/qemu.pid",
	}.args(), " ")
	for _, want := range []string{
		"-machine q35,accel=kvm", "-smp 4", "-m 16384",
		"file=/v/jit.raw,if=none,id=jit,format=raw,readonly=on",
		"-device virtio-blk-pci,drive=jit,serial=eph-jit",
		"-netdev user,id=net0", "-no-reboot", "-sandbox on,",
		"-pidfile /v/qemu.pid", "file=/v/disk.qcow2,if=none,id=root", "drive=root,bootindex=0",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("qemu args missing %q:\n%s", want, args)
		}
	}
}

func TestCloudConfigCarriesGuestFiles(t *testing.T) {
	data, err := cloudConfig("26.04", runnerRelease{Version: "2.338.0", SHA256: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.HasPrefix(s, "#cloud-config\n") {
		t.Fatal("missing #cloud-config header")
	}
	for _, want := range []string{
		"/var/lib/eph-build/guest/provision.sh", "/var/lib/eph-build/guest/eph-runner.service",
		"/var/lib/eph-build/guest/eph-runner-prepare", "/var/lib/eph-build/guest/eph-runner-start",
		`provision.sh, "26.04", "2.338.0", "abc"]`, "power_state:", "users: []",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("cloud-config missing %q", want)
		}
	}
	// The guest and host must agree on how the registration arrives.
	rule, _ := guestFiles.ReadFile("images/90-eph-runner.rules")
	if !strings.Contains(string(rule), `ATTRS{serial}=="`+jitDiskSerial+`"`) || !strings.Contains(string(rule), "eph-runner.service") {
		t.Error("the udev rule doesn't match the registration disk the host attaches")
	}
	if !strings.Contains(s, "/var/lib/eph-build/guest/90-eph-runner.rules") {
		t.Error("cloud-config is missing the udev rule")
	}
}

func TestJITDisk(t *testing.T) {
	d := jitDisk("abc")
	if len(d) != 512 || string(d[:4]) != "abc\n" || d[511] != 0 {
		t.Fatalf("jitDisk = %d bytes, starts %q", len(d), d[:4])
	}
	if len(jitDisk(strings.Repeat("x", 600))) != 1024 {
		t.Fatal("expected two sectors")
	}
}

func TestParseRunnerRelease(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	rel, err := parseRunnerRelease([]byte(`{"tag_name":"v2.338.0","body":"- actions-runner-linux-x64-2.338.0.tar.gz <!-- BEGIN SHA linux-x64 -->` + sha + `<!-- END SHA linux-x64 -->"}`))
	if err != nil || rel.Version != "2.338.0" || rel.SHA256 != sha {
		t.Fatalf("parseRunnerRelease = %+v, %v", rel, err)
	}
	if _, err := parseRunnerRelease([]byte(`{"tag_name":"v2.338.0","body":"no checksums"}`)); err == nil {
		t.Fatal("expected an error without a linux-x64 SHA")
	}
}

func TestSumFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SHA256SUMS")
	os.WriteFile(path, []byte("aaa *ubuntu-24.04-server-cloudimg-amd64.img\nbbb *ubuntu-24.04-server-cloudimg-arm64.img\n"), 0o600)
	if got, err := sumFor(path, "ubuntu-24.04-server-cloudimg-amd64.img"); err != nil || got != "aaa" {
		t.Fatalf("sumFor = %q, %v", got, err)
	}
	if _, err := sumFor(path, "missing.img"); err == nil {
		t.Fatal("expected an error for an unlisted file")
	}
}
