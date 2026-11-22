package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// The VM launcher is this binary's vm command, which systemd runs once per
// connection to ephemeral-runners-vm.socket (see
// packaging/ephemeral-runners-vm@.service), as a user of its own, never the
// controller's. It gets one request on the connection, as one message: run a
// VM, with its files' descriptors attached, or probe what its user can
// reach. The controller's privileges stay its own, and the VM user reaches
// the controller only through this connection.
//
// Replies are messages too, each starting with its kind: msgOutput carries
// QEMU's console, msgResult the outcome as JSON. The guest writes the
// console, so it is never parsed here or mistaken for a result.
const (
	msgOutput = 'o'
	msgResult = 'r'
	// maxVMMessage bounds every message either way.
	maxVMMessage = 64 << 10
	// maxVMFiles bounds the descriptors a request may carry.
	maxVMFiles = 8
	// vmStopGrace is how long QEMU has to exit after SIGTERM.
	vmStopGrace = 10 * time.Second
)

type vmRequest struct {
	Run *vmRun `json:"run,omitempty"`
	// Probe lists paths for the launcher to try to read.
	Probe []string `json:"probe,omitempty"`
}

// vmRun is a VMSpec on the wire: its files are the descriptors that come
// with the request.
type vmRun struct {
	Bin       string   `json:"bin"`
	Name      string   `json:"name"`
	CPU       int      `json:"cpu"`
	MemoryMB  int      `json:"memoryMB"`
	Backing   bool     `json:"backing,omitempty"`
	JIT       bool     `json:"jit,omitempty"`
	CDROM     bool     `json:"cdrom,omitempty"`
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

type vmResult struct {
	// Error is why QEMU failed to start or exited unsuccessfully.
	Error string `json:"error,omitempty"`
}

// vmProbe is what the VM user is and can reach.
type vmProbe struct {
	UID int `json:"uid"`
	// KVM is why /dev/kvm can't be opened, if it can't.
	KVM string `json:"kvm,omitempty"`
	// Readable are the probed paths the VM user could open.
	Readable []string `json:"readable,omitempty"`
}

// serveVMStdin serves the connection systemd passed as standard input.
func serveVMStdin() error {
	c, err := net.FileConn(os.Stdin)
	if err != nil {
		return fmt.Errorf("standard input is not the VM launcher's socket: %w", err)
	}
	conn, ok := c.(*net.UnixConn)
	if !ok {
		return errors.New("standard input is not the VM launcher's socket")
	}
	return serveVM(conn)
}

// serveVM answers the one request that arrives on conn.
func serveVM(conn *net.UnixConn) error {
	defer conn.Close()
	buf := make([]byte, maxVMMessage)
	oob := make([]byte, syscall.CmsgSpace(maxVMFiles*4))
	n, oobn, flags, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	files, err := receivedFiles(oob[:oobn])
	defer closeAll(files)
	if err != nil {
		return err
	}
	if flags&(syscall.MSG_TRUNC|syscall.MSG_CTRUNC) != 0 {
		return errors.New("request too large")
	}
	var req vmRequest
	if err := json.Unmarshal(buf[:n], &req); err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	switch {
	case req.Run != nil:
		return runVMProcess(conn, *req.Run, files)
	case req.Probe != nil:
		return sendResult(conn, probeVMUser(req.Probe))
	}
	return errors.New("empty request")
}

func receivedFiles(oob []byte) ([]*os.File, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var files []*os.File
	for _, m := range msgs {
		fds, err := syscall.ParseUnixRights(&m)
		if err != nil {
			return files, err
		}
		for _, fd := range fds {
			files = append(files, os.NewFile(uintptr(fd), "vm-file"))
		}
	}
	return files, nil
}

// runVMProcess runs QEMU until it exits or the controller hangs up, then
// reports how it ended.
func runVMProcess(conn *net.UnixConn, run vmRun, files []*os.File) error {
	if len(files) != run.files() {
		return sendResult(conn, vmResult{Error: fmt.Sprintf("got %d files for a VM that needs %d", len(files), run.files())})
	}
	cmd := exec.Command(run.Bin, run.args()...)
	cmd.ExtraFiles = files
	out := &outputWriter{conn: conn}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return sendResult(conn, vmResult{Error: fmt.Sprintf("start qemu: %v", err)})
	}
	closeAll(files)

	exited := make(chan struct{})
	go func() {
		// The controller sends nothing more: the connection ending, or
		// anything else arriving, means it wants the VM gone or is gone
		// itself.
		_, _ = conn.Read(make([]byte, 1))
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(vmStopGrace):
			_ = cmd.Process.Kill()
		}
	}()
	err := cmd.Wait()
	close(exited)
	var res vmResult
	if err != nil {
		res.Error = err.Error()
	}
	return sendResult(conn, res)
}

func sendResult(conn *net.UnixConn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = conn.Write(append([]byte{msgResult}, data...))
	return err
}

// outputWriter sends what it's given as msgOutput messages.
type outputWriter struct {
	conn *net.UnixConn
}

func (w *outputWriter) Write(p []byte) (int, error) {
	const chunk = 16 << 10
	for written := 0; written < len(p); {
		n := min(chunk, len(p)-written)
		if _, err := w.conn.Write(append([]byte{msgOutput}, p[written:written+n]...)); err != nil {
			return written, err
		}
		written += n
	}
	return len(p), nil
}

func probeVMUser(paths []string) vmProbe {
	p := vmProbe{UID: os.Getuid()}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		p.KVM = err.Error()
	} else {
		f.Close()
	}
	for _, path := range paths {
		if f, err := os.Open(path); err == nil {
			f.Close()
			p.Readable = append(p.Readable, path)
		}
	}
	return p
}
