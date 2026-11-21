package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestVMRunArgs(t *testing.T) {
	run := vmRun{Name: "eph-acme-x-1", CPU: 4, MemoryMB: 16384, Backing: true, JIT: true, ExtraArgs: []string{"-x"}}
	args := strings.Join(run.args(), " ")
	for _, want := range []string{
		"-machine q35,accel=kvm", "-smp 4", "-m 16384",
		"-add-fd fd=3,set=1 -add-fd fd=4,set=1",
		"-add-fd fd=5,set=2", "file=/dev/fdset/1,if=none,id=root,format=qcow2,",
		",backing.driver=qcow2,backing.file.filename=/dev/fdset/2", "drive=root,bootindex=0",
		"-add-fd fd=6,set=3 -drive file=/dev/fdset/3,if=none,id=jit,format=raw,readonly=on",
		"-device virtio-blk-pci,drive=jit,serial=eph-jit",
		"-netdev user,id=net0", "-no-reboot", "-sandbox on,",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("qemu args missing %q:\n%s", want, args)
		}
	}
	if !strings.HasSuffix(args, " -x") {
		t.Errorf("extra args don't come last:\n%s", args)
	}
	if run.files() != 4 {
		t.Errorf("files() = %d, want 4", run.files())
	}

	// An image build: a standalone disk and the seed.
	build := strings.Join(vmRun{CDROM: true}.args(), " ")
	if strings.Contains(build, "backing") || !strings.Contains(build, "-add-fd fd=5,set=4 -drive file=/dev/fdset/4,media=cdrom,readonly=on") {
		t.Errorf("build VM args:\n%s", build)
	}
}

func TestVMProbeCheck(t *testing.T) {
	for _, tc := range []struct {
		probe vmProbe
		want  string
	}{
		{vmProbe{UID: 61234}, ""},
		{vmProbe{UID: 0}, "as root"},
		{vmProbe{UID: 999}, "the controller's"},
		{vmProbe{UID: 61234, KVM: "permission denied"}, "can't use KVM"},
		{vmProbe{UID: 61234, Readable: []string{"/etc/ephemeral-runners/keys/acme.pem"}}, "could read /etc/ephemeral-runners/keys/acme.pem"},
	} {
		err := tc.probe.check(999)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%+v: %v", tc.probe, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%+v: got %v, want %q", tc.probe, err, tc.want)
		}
	}
}

func TestPrivatePaths(t *testing.T) {
	cfg := &Config{Dir: "/etc/er", Deployments: []*Deployment{
		{Path: "/etc/er/deployments/acme.toml", GitHubApp: &AppConfig{PrivateKeyPath: "/etc/er/keys/acme.pem"}},
		{Path: "/etc/er/deployments/beta.toml", GitHubApp: &AppConfig{PrivateKeyPath: "/etc/er/keys/beta.pem"}},
	}}
	got := strings.Join(cfg.PrivatePaths(), " ")
	want := "/etc/er /etc/er/deployments /etc/er/deployments/acme.toml /etc/er/deployments/beta.toml /etc/er/host.toml /etc/er/keys /etc/er/keys/acme.pem /etc/er/keys/beta.pem /etc/er/preload.toml"
	if got != want {
		t.Errorf("PrivatePaths:\n got %s\nwant %s", got, want)
	}
}

// launcher stands in for ephemeral-runners-vm.socket: it serves each
// connection the way an ephemeral-runners-vm@ instance would.
func launcher(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the VM launcher uses Linux's SOCK_SEQPACKET")
	}
	sock := filepath.Join(t.TempDir(), "vm.sock")
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: sock, Net: "unixpacket"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			conn, err := l.AcceptUnix()
			if err != nil {
				return
			}
			wg.Go(func() {
				if err := serveVM(conn); err != nil {
					t.Log("launcher:", err)
				}
			})
		}
	})
	return sock
}

// fakeQEMU writes a script that stands in for qemu-system-x86_64.
func fakeQEMU(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qemu")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// vmFiles makes a VM's files the way the controller would: only its own
// user can open them, so QEMU gets them only as descriptors.
func vmFiles(t *testing.T) VMSpec {
	t.Helper()
	dir := t.TempDir()
	spec := VMSpec{Name: "eph-acme-x-1", CPU: 2, MemoryMB: 512,
		Disk: filepath.Join(dir, "disk"), Backing: filepath.Join(dir, "base"), JITDisk: filepath.Join(dir, "jit")}
	for path, data := range map[string]string{spec.Disk: "empty\n", spec.Backing: "golden\n", spec.JITDisk: "registration\n"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return spec
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestVMLauncherRunsQEMU(t *testing.T) {
	sock := launcher(t)
	// Reads the golden image and the registration through their
	// descriptors, writes the disk through its own, and prints its console
	// on stdout and stderr.
	bin := fakeQEMU(t, `read -r golden <&5; read -r jit <&6
echo "golden: $golden, jit: $jit"
echo written >&3
echo "Listening for Jobs" >&2
`)
	spec := vmFiles(t)
	q := &QEMU{Bin: bin, Socket: sock}
	var out syncBuffer
	vm, err := q.Start(spec, &out)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("VM didn't finish")
	}
	if err := vm.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "golden: golden, jit: registration\n") || !strings.Contains(got, "Listening for Jobs") {
		t.Errorf("console = %q", got)
	}
	if data, _ := os.ReadFile(spec.Disk); string(data) != "written\n" {
		t.Errorf("disk = %q; QEMU should have written it through its descriptor", data)
	}
	vm.Stop() // a VM that's gone is a no-op
}

func TestVMLauncherReportsQEMUFailure(t *testing.T) {
	sock := launcher(t)
	q := &QEMU{Bin: fakeQEMU(t, "echo 'no KVM' >&2; exit 3\n"), Socket: sock}
	var out syncBuffer
	vm, err := q.Start(vmFiles(t), &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Err(); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("Err() = %v, want exit status 3", err)
	}
	if !strings.Contains(out.String(), "no KVM") {
		t.Errorf("console = %q", out.String())
	}

	q.Bin = filepath.Join(t.TempDir(), "missing")
	vm, err = q.Start(vmFiles(t), &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Err(); err == nil || !strings.Contains(err.Error(), "start qemu") {
		t.Errorf("Err() = %v, want start qemu", err)
	}
}

func TestVMLauncherStopsQEMU(t *testing.T) {
	sock := launcher(t)
	q := &QEMU{Bin: fakeQEMU(t, "echo up; exec sleep 60\n"), Socket: sock}
	var out syncBuffer
	vm, err := q.Start(vmFiles(t), &out)
	if err != nil {
		t.Fatal(err)
	}
	for !strings.Contains(out.String(), "up") {
		select {
		case <-vm.Done():
			t.Fatalf("VM exited early: %v", vm.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	start := time.Now()
	vm.Stop()
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Stop took %s; SIGTERM should end it at once", took)
	}
	if err := vm.Err(); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Errorf("Err() = %v, want terminated", err)
	}
}

func TestVMLauncherProbe(t *testing.T) {
	sock := launcher(t)
	dir := t.TempDir()
	readable := filepath.Join(dir, "readable")
	if err := os.WriteFile(readable, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	q := &QEMU{Socket: sock}
	// The launcher here runs as this test's user, as it would if the VM
	// user were the controller's, so check must refuse it.
	err := q.checkVMUser([]string{filepath.Join(dir, "missing"), readable})
	if err == nil || !(strings.Contains(err.Error(), "the controller's") || strings.Contains(err.Error(), "as root")) {
		t.Errorf("checkVMUser = %v, want it refused", err)
	}

	p := probeVMUser([]string{filepath.Join(dir, "missing"), readable})
	if p.UID != os.Getuid() || strings.Join(p.Readable, " ") != readable {
		t.Errorf("probe = %+v", p)
	}
}

func TestVMLauncherUnreachable(t *testing.T) {
	q := &QEMU{Socket: filepath.Join(t.TempDir(), "none.sock")}
	if err := q.checkVMUser(nil); err == nil || !strings.Contains(err.Error(), "ephemeral-runners-vm.socket") {
		t.Errorf("checkVMUser = %v", err)
	}
}
