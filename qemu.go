package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
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

// vmSocket is where ephemeral-runners-vm.socket listens. systemd starts an
// ephemeral-runners-vm@ instance for each connection, under a user of its
// own, and that instance runs one VM's QEMU (see vm.go).
const vmSocket = "/run/ephemeral-runners/vm.sock"

// QEMU runs job VMs with qemu-system-x86_64 and KVM.
type QEMU struct {
	Bin    string
	ImgBin string
	// Socket is the VM launcher's, vmSocket outside tests.
	Socket string
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
	return &QEMU{Bin: path, ImgBin: img, Socket: vmSocket}, nil
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

// VMSpec is one VM to boot. The controller opens its files and QEMU gets
// them as file descriptors, so QEMU's user needs no access to their paths.
type VMSpec struct {
	Name     string
	CPU      int
	MemoryMB int
	// Disk is the boot disk, which the VM writes to. Backing, when set, is
	// the golden image Disk is an overlay on, which the VM only reads.
	Disk    string
	Backing string
	// JITDisk, when set, is a raw file attached read-only as the guest's
	// registration disk (see jitDiskSerial).
	JITDisk string
	// CDROM, when set, attaches an ISO (the image build's cloud-init seed).
	CDROM     string
	ExtraArgs []string
}

// open opens spec's files in the order vmRun.args numbers them.
func (s VMSpec) open() (vmRun, []*os.File, error) {
	run := vmRun{
		Name: s.Name, CPU: s.CPU, MemoryMB: s.MemoryMB, ExtraArgs: s.ExtraArgs,
		Backing: s.Backing != "", JIT: s.JITDisk != "", CDROM: s.CDROM != "",
	}
	var files []*os.File
	open := func(path string, flag int) error {
		f, err := os.OpenFile(path, flag, 0)
		if err == nil {
			files = append(files, f)
		}
		return err
	}
	// QEMU opens the disk read-only before it opens it to write, so it
	// needs a descriptor for each.
	err := open(s.Disk, os.O_RDWR)
	if err == nil {
		err = open(s.Disk, os.O_RDONLY)
	}
	for _, p := range []string{s.Backing, s.JITDisk, s.CDROM} {
		if p != "" && err == nil {
			err = open(p, os.O_RDONLY)
		}
	}
	if err != nil {
		closeAll(files)
		return vmRun{}, nil, err
	}
	return run, files, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}

// VM is a running QEMU process, reached through its connection to the VM
// launcher.
type VM struct {
	conn *net.UnixConn
	done chan struct{}
	err  error
}

// Start boots the VM. Its console goes to out. Done closes once QEMU has
// exited, which happens when the guest powers off.
func (q *QEMU) Start(spec VMSpec, out io.Writer) (*VM, error) {
	run, files, err := spec.open()
	if err != nil {
		return nil, fmt.Errorf("open VM files: %w", err)
	}
	defer closeAll(files)
	run.Bin = q.Bin
	conn, err := q.request(vmRequest{Run: &run}, files)
	if err != nil {
		return nil, fmt.Errorf("start qemu: %w", err)
	}
	vm := &VM{conn: conn, done: make(chan struct{})}
	go vm.read(out)
	return vm, nil
}

// request sends req, with files, to a new VM launcher.
func (q *QEMU) request(req vmRequest, files []*os.File) (*net.UnixConn, error) {
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: q.Socket, Net: "unixpacket"})
	if err != nil {
		return nil, fmt.Errorf("can't reach the VM launcher (is ephemeral-runners-vm.socket running? install.sh sets it up): %w", err)
	}
	data, err := json.Marshal(req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	fds := make([]int, len(files))
	for i, f := range files {
		fds[i] = int(f.Fd())
	}
	var oob []byte
	if len(fds) > 0 {
		oob = syscall.UnixRights(fds...)
	}
	if _, _, err := conn.WriteMsgUnix(data, oob, nil); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// read passes the VM's console to out until the launcher reports QEMU's
// exit and hangs up.
func (v *VM) read(out io.Writer) {
	defer close(v.done)
	defer v.conn.Close()
	buf := make([]byte, maxVMMessage)
	reported := false
	for {
		n, err := v.conn.Read(buf)
		if err != nil {
			if !reported {
				v.err = fmt.Errorf("the VM launcher hung up without reporting QEMU's exit: %w", err)
			}
			return
		}
		switch msg := buf[:n]; msg[0] {
		case msgOutput:
			_, _ = out.Write(msg[1:])
		case msgResult:
			var res vmResult
			if err := json.Unmarshal(msg[1:], &res); err != nil {
				v.err = fmt.Errorf("the VM launcher's report: %w", err)
			} else if res.Error != "" {
				v.err = errors.New(res.Error)
			}
			reported = true
		}
	}
}

// Done is closed once QEMU has exited.
func (v *VM) Done() <-chan struct{} { return v.done }

// Err is QEMU's exit status. It waits for QEMU to exit.
func (v *VM) Err() error {
	<-v.done
	return v.err
}

// stopWait is how long Stop waits for the launcher, which gives QEMU
// vmStopGrace before killing it.
const stopWait = vmStopGrace + 10*time.Second

// Stop ends the VM: the launcher sends QEMU SIGTERM, which it treats like
// pulling the power, and SIGKILL if it hasn't exited after a few seconds.
// Safe to call repeatedly.
func (v *VM) Stop() {
	if v == nil {
		return
	}
	select {
	case <-v.done:
		return
	default:
	}
	_ = v.conn.CloseWrite()
	select {
	case <-v.done:
	case <-time.After(stopWait):
		_ = v.conn.Close()
		<-v.done
	}
}

// checkVMUser fails unless VMs would run as a user other than this process's
// and root, which can use KVM but read none of paths. Paths that don't exist
// count as unreadable.
func (q *QEMU) checkVMUser(paths []string) error {
	conn, err := q.request(vmRequest{Probe: paths}, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	buf := make([]byte, maxVMMessage)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("the VM launcher didn't answer: %w", err)
	}
	var p vmProbe
	if n == 0 || buf[0] != msgResult {
		return errors.New("the VM launcher answered out of turn")
	}
	if err := json.Unmarshal(buf[1:n], &p); err != nil {
		return fmt.Errorf("the VM launcher's answer: %w", err)
	}
	return p.check(os.Getuid())
}

// check fails unless the probed user is neither root nor self, can use KVM
// and could read nothing it was asked to try.
func (p vmProbe) check(self int) error {
	switch {
	case p.UID == 0:
		return errors.New("VMs would run as root; they must run under a user of their own (see the security model in README.md)")
	case p.UID == self:
		return fmt.Errorf("VMs would run as this user (uid %d), the controller's; they must run under a user of their own (see the security model in README.md)", p.UID)
	case p.KVM != "":
		return fmt.Errorf("VMs can't use KVM (is their user in the kvm group, and virtualization enabled in firmware?): %s", p.KVM)
	case len(p.Readable) > 0:
		return fmt.Errorf("VMs could read %s, which only root and the controller's user may", strings.Join(p.Readable, ", "))
	}
	return nil
}

// args numbers the descriptors in the order VMSpec.open opens them: QEMU
// gets them as 3 onwards.
func (r vmRun) args() []string {
	next := 3
	fd := func() string {
		next++
		return strconv.Itoa(next - 1)
	}
	disk := "fd=" + fd() + ",set=1"
	diskRO := "fd=" + fd() + ",set=1"
	args := []string{
		"-name", r.Name + ",process=" + r.Name,
		"-machine", "q35,accel=kvm",
		"-cpu", "host",
		"-smp", strconv.Itoa(r.CPU),
		"-m", strconv.Itoa(r.MemoryMB),
		"-nodefaults", "-no-user-config",
		"-display", "none",
		// The guest's console (kernel, systemd and the runner) is QEMU's
		// stdout. signal=off keeps Ctrl-C on a terminal from reaching it.
		"-chardev", "stdio,id=console,signal=off",
		"-serial", "chardev:console",
		"-add-fd", disk, "-add-fd", diskRO,
	}
	// The disk is thrown away with the VM, so flushing to the host buys
	// nothing. It's the boot disk explicitly: with a second disk attached,
	// the firmware would otherwise try that one first.
	root := "file=/dev/fdset/1,if=none,id=root,format=qcow2,cache=unsafe,discard=unmap,detect-zeroes=unmap"
	if r.Backing {
		// The overlay names the golden image by path, which QEMU's user
		// can't open; it gets the image's descriptor instead.
		args = append(args, "-add-fd", "fd="+fd()+",set=2")
		root += ",backing.driver=qcow2,backing.file.filename=/dev/fdset/2"
	}
	args = append(args,
		"-drive", root,
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
	)
	if r.JIT {
		args = append(args,
			"-add-fd", "fd="+fd()+",set=3",
			"-drive", "file=/dev/fdset/3,if=none,id=jit,format=raw,readonly=on",
			"-device", "virtio-blk-pci,drive=jit,serial="+jitDiskSerial)
	}
	if r.CDROM {
		args = append(args,
			"-add-fd", "fd="+fd()+",set=4",
			"-drive", "file=/dev/fdset/4,media=cdrom,readonly=on")
	}
	return append(args, r.ExtraArgs...)
}

// files is how many descriptors args expects.
func (r vmRun) files() int {
	n := 2
	for _, b := range []bool{r.Backing, r.JIT, r.CDROM} {
		if b {
			n++
		}
	}
	return n
}
