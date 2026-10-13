package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// jitDiskSerial identifies the small read-only disk that carries a VM's
// runner registration. A udev rule in the image (images/90-eph-runner.rules)
// matches it and starts the one-job service. A disk needs nothing from the
// guest kernel that booting doesn't already: QEMU's fw_cfg device would,
// but Ubuntu cloud images leave its driver out.
const jitDiskSerial = "eph-jit"

// QEMU runs job VMs with qemu-system-x86_64 and KVM.
type QEMU struct {
	Bin    string
	ImgBin string
}

func newQEMU(bin string) (*QEMU, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%s not found: install qemu-system-x86: %w", bin, err)
	}
	img, err := exec.LookPath("qemu-img")
	if err != nil {
		return nil, fmt.Errorf("qemu-img not found: install qemu-utils: %w", err)
	}
	return &QEMU{Bin: path, ImgBin: img}, nil
}

// CheckKVM fails unless this process can open /dev/kvm.
func CheckKVM() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("can't use KVM (is the user in the kvm group, and virtualization enabled in firmware?): %w", err)
	}
	return f.Close()
}

func (q *QEMU) img(ctx context.Context, args ...string) error {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, q.ImgBin, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("qemu-img %s: %w: %s", args[0], err, strings.TrimSpace(out.String()))
	}
	return nil
}

// CreateOverlay makes a copy-on-write disk on top of a golden image. It is
// instant and starts empty: the VM's writes land in it, and the golden image
// is never changed.
func (q *QEMU) CreateOverlay(ctx context.Context, image, disk string) error {
	return q.img(ctx, "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", image, disk)
}

// VMSpec is one VM to boot.
type VMSpec struct {
	Name     string
	Disk     string
	CPU      int
	MemoryMB int
	// JITDisk, when set, is a raw file attached read-only as the guest's
	// registration disk (see jitDiskSerial).
	JITDisk string
	// CDROM, when set, attaches an ISO (the image build's cloud-init seed).
	CDROM     string
	ExtraArgs []string
	// PIDFile records the QEMU process, so leftovers can be found.
	PIDFile string
}

func (s VMSpec) args() []string {
	args := []string{
		"-name", s.Name + ",process=" + s.Name,
		"-machine", "q35,accel=kvm",
		"-cpu", "host",
		"-smp", strconv.Itoa(s.CPU),
		"-m", strconv.Itoa(s.MemoryMB),
		"-nodefaults", "-no-user-config",
		"-display", "none",
		// The guest's console (kernel, systemd and the runner) is QEMU's
		// stdout. signal=off keeps Ctrl-C on a terminal from reaching it.
		"-chardev", "stdio,id=console,signal=off",
		"-serial", "chardev:console",
		// The disk is thrown away with the VM, so flushing to the host
		// buys nothing. It's the boot disk explicitly: with a second disk
		// attached, the firmware would otherwise try that one first.
		"-drive", "file=" + s.Disk + ",if=none,id=root,format=qcow2,cache=unsafe,discard=unmap,detect-zeroes=unmap",
		"-device", "virtio-blk-pci,drive=root,bootindex=0",
		// User-mode networking: the VM shares no network with the host or
		// other VMs, and its traffic leaves as this process's own, which
		// the host firewall restricts (see packaging/firewall.nft).
		"-netdev", "user,id=net0",
		"-device", "virtio-net-pci,netdev=net0",
		"-device", "virtio-rng-pci",
		// A guest reboot ends the VM instead of starting it again.
		"-no-reboot",
		// Seccomp: QEMU may not spawn processes, gain privileges or use
		// obsolete syscalls, which limits a guest that escapes into it.
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
	}
	if s.JITDisk != "" {
		args = append(args,
			"-drive", "file="+s.JITDisk+",if=none,id=jit,format=raw,readonly=on",
			"-device", "virtio-blk-pci,drive=jit,serial="+jitDiskSerial)
	}
	if s.CDROM != "" {
		args = append(args, "-drive", "file="+s.CDROM+",media=cdrom,readonly=on")
	}
	if s.PIDFile != "" {
		args = append(args, "-pidfile", s.PIDFile)
	}
	return append(args, s.ExtraArgs...)
}

// VM is a running QEMU process.
type VM struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// Start boots the VM. Its console goes to out. Done closes once QEMU has
// exited, which happens when the guest powers off.
func (q *QEMU) Start(spec VMSpec, out io.Writer) (*VM, error) {
	cmd := exec.Command(q.Bin, spec.args()...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start qemu: %w", err)
	}
	vm := &VM{cmd: cmd, done: make(chan struct{})}
	go func() {
		vm.err = cmd.Wait()
		close(vm.done)
	}()
	return vm, nil
}

// Done is closed once QEMU has exited.
func (v *VM) Done() <-chan struct{} { return v.done }

// Err is QEMU's exit status. It waits for QEMU to exit.
func (v *VM) Err() error {
	<-v.done
	return v.err
}

// Stop ends the VM: SIGTERM, which QEMU treats like pulling the power, and
// SIGKILL if it hasn't exited after a few seconds. Safe to call repeatedly.
func (v *VM) Stop() {
	if v == nil {
		return
	}
	select {
	case <-v.done:
		return
	default:
	}
	_ = v.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-v.done:
	case <-time.After(10 * time.Second):
		_ = v.cmd.Process.Kill()
		<-v.done
	}
}

// killLeftover ends a QEMU process from an earlier run, found through its
// pidfile, if it is still running and still that VM.
func killLeftover(pidFile, name string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil || !bytes.Contains(cmdline, []byte(name+",process="+name)) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
