package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed images/provision.sh images/preload.sh images/eph-runner.service images/eph-runner-prepare images/eph-runner-start images/90-eph-runner.rules
var guestFiles embed.FS

const (
	cloudImageBase    = "https://cloud-images.ubuntu.com/releases"
	cloudImageKeyring = "/usr/share/keyrings/ubuntu-cloudimage-keyring.gpg"
	buildTimeout      = 45 * time.Minute
	buildCPU          = 4
	buildMemoryMB     = 8192
)

var ubuntuImageRE = regexp.MustCompile(`^ubuntu-(\d{2}\.\d{2})$`)

// buildImage makes state_dir/images/<name>.qcow2 from the official Ubuntu
// cloud image, provisioned inside a throwaway VM. The new image replaces the
// old one atomically: VMs already running keep the file they opened, and
// new VMs use the new one.
func buildImage(ctx context.Context, log *slog.Logger, q *QEMU, h HostConfig, name string, diskGB int) error {
	m := ubuntuImageRE.FindStringSubmatch(name)
	if m == nil {
		return fmt.Errorf("only ubuntu-<version> images can be built, e.g. ubuntu-24.04; put a custom image at %s yourself", h.ImagePath(name))
	}
	version := m[1]
	log = log.With(slog.String("image", name))
	start := time.Now()

	imagesDir := filepath.Dir(h.ImagePath(name))
	cacheDir := filepath.Join(imagesDir, "cache")
	buildDir := filepath.Join(imagesDir, ".build-"+name)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	_ = os.RemoveAll(buildDir)
	if err := os.Mkdir(buildDir, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(buildDir)

	base, err := fetchCloudImage(ctx, log, cacheDir, version)
	if err != nil {
		return err
	}
	runner, err := latestRunner(ctx, log, cacheDir)
	if err != nil {
		return err
	}

	disk := filepath.Join(buildDir, "disk.qcow2")
	if err := q.img(ctx, "convert", "-q", "-O", "qcow2", base, disk); err != nil {
		return err
	}
	if err := q.img(ctx, "resize", "-q", disk, fmt.Sprintf("%dG", diskGB)); err != nil {
		return err
	}
	seed, err := writeSeed(ctx, buildDir, name, version, runner, h.Preload)
	if err != nil {
		return err
	}

	log.Info("Provisioning image", slog.String("ubuntu", version), slog.String("runner", runner.Version), slog.Int("diskGB", diskGB),
		slog.Any("preloadPlaywright", h.Preload.Playwright), slog.Int("preloadDockerImages", len(h.Preload.DockerImages)))
	console, err := os.Create(filepath.Join(buildDir, "console.log"))
	if err != nil {
		return err
	}
	defer console.Close()
	ok := make(chan struct{})
	vm, err := q.Start(VMSpec{
		Name:     h.VMPrefix + "build-" + strings.ReplaceAll(name, ".", ""),
		Disk:     disk,
		CPU:      buildCPU,
		MemoryMB: buildMemoryMB,
		CDROM:    seed,
	}, &watchWriter{w: console, needle: []byte("EPH-BUILD-OK"), found: ok})
	if err != nil {
		return err
	}
	defer vm.Stop()
	// Keep the build's console next to the image, success or not; the
	// build directory itself goes.
	buildLog := filepath.Join(imagesDir, name+".build.log")
	defer func() { _ = copyFile(console.Name(), buildLog) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(buildTimeout):
		return fmt.Errorf("provisioning didn't finish within %s; full log in %s; last provisioning output:\n%s", buildTimeout, buildLog, provisionTail(console.Name()))
	case <-vm.Done():
	}
	select {
	case <-ok:
	default:
		return fmt.Errorf("provisioning failed; full log in %s; last provisioning output:\n%s", buildLog, provisionTail(console.Name()))
	}
	// Preloading only saves jobs time, so an item that failed is left out
	// rather than failing the build; jobs download it as before.
	for _, item := range preloadFailures(console.Name()) {
		log.Warn("Preload failed; the image goes without it", slog.String("item", item), slog.String("log", buildLog))
	}

	// Flatten into a standalone image without the zeroed blocks.
	tmp := h.ImagePath(name) + ".new"
	if err := q.img(ctx, "convert", "-q", "-O", "qcow2", disk, tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, h.ImagePath(name)); err != nil {
		return err
	}
	log.Info("Image ready", slog.String("path", h.ImagePath(name)), slog.Duration("took", time.Since(start).Round(time.Second)))
	return nil
}

// fetchCloudImage downloads Ubuntu's cloud image for version into cacheDir,
// unless the cached copy still matches, and verifies it against the
// SHA256SUMS that Ubuntu's cloud image key signs.
func fetchCloudImage(ctx context.Context, log *slog.Logger, cacheDir, version string) (string, error) {
	file := "ubuntu-" + version + "-server-cloudimg-amd64.img"
	url := cloudImageBase + "/" + version + "/release/"
	sums := filepath.Join(cacheDir, "SHA256SUMS-"+version)
	sig := sums + ".gpg"
	if err := download(ctx, url+"SHA256SUMS", sums); err != nil {
		return "", err
	}
	if err := download(ctx, url+"SHA256SUMS.gpg", sig); err != nil {
		return "", err
	}
	if out, err := exec.CommandContext(ctx, "gpgv", "--keyring", cloudImageKeyring, sig, sums).CombinedOutput(); err != nil {
		return "", fmt.Errorf("SHA256SUMS signature check failed (is ubuntu-cloudimage-keyring installed?): %v: %s", err, bytes.TrimSpace(out))
	}
	want, err := sumFor(sums, file)
	if err != nil {
		return "", err
	}
	path := filepath.Join(cacheDir, file)
	if got, err := sha256File(path); err == nil && got == want {
		log.Info("Cloud image up to date", slog.String("file", file))
		return path, nil
	}
	log.Info("Downloading cloud image", slog.String("url", url+file))
	if err := download(ctx, url+file, path); err != nil {
		return "", err
	}
	got, err := sha256File(path)
	if err != nil {
		return "", err
	}
	if got != want {
		os.Remove(path)
		return "", fmt.Errorf("%s: SHA-256 %s doesn't match SHA256SUMS (%s)", file, got, want)
	}
	return path, nil
}

func download(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("GET %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// sumFor finds file's hash in a SHA256SUMS listing ("<hash> *<file>").
func sumFor(sumsPath, file string) (string, error) {
	data, err := os.ReadFile(sumsPath)
	if err != nil {
		return "", err
	}
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		hash, name, ok := strings.Cut(s.Text(), " ")
		if ok && strings.TrimLeft(name, " *") == file {
			return hash, nil
		}
	}
	return "", fmt.Errorf("%s isn't listed in SHA256SUMS", file)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeSeed makes the cloud-init NoCloud seed ISO that provisions the build
// VM: it writes the guest files, runs provision.sh, then powers off.
func writeSeed(ctx context.Context, dir, name, version string, runner runnerRelease, preload Preload) (string, error) {
	userData, err := cloudConfig(version, runner, preload)
	if err != nil {
		return "", err
	}
	ud := filepath.Join(dir, "user-data")
	md := filepath.Join(dir, "meta-data")
	if err := os.WriteFile(ud, userData, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(md, []byte("instance-id: eph-build-"+name+"\nlocal-hostname: eph-build\n"), 0o600); err != nil {
		return "", err
	}
	seed := filepath.Join(dir, "seed.iso")
	if out, err := exec.CommandContext(ctx, "cloud-localds", seed, ud, md).CombinedOutput(); err != nil {
		return "", fmt.Errorf("cloud-localds (install cloud-image-utils): %v: %s", err, bytes.TrimSpace(out))
	}
	return seed, nil
}

func cloudConfig(version string, runner runnerRelease, preload Preload) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("#cloud-config\n")
	// No default "ubuntu" user, no password logins: nothing logs in to
	// these VMs.
	b.WriteString("users: []\nssh_pwauth: false\npackage_update: false\npackage_upgrade: false\n")
	b.WriteString("write_files:\n")
	entries, err := guestFiles.ReadDir("images")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		data, err := guestFiles.ReadFile("images/" + e.Name())
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "  - path: /var/lib/eph-build/guest/%s\n    permissions: '0755'\n    encoding: b64\n    content: %s\n",
			e.Name(), base64.StdEncoding.EncodeToString(data))
	}
	// Always written, empty when nothing is preloaded, so provision.sh
	// can read it unconditionally.
	fmt.Fprintf(&b, "  - path: /var/lib/eph-build/preload\n    permissions: '0644'\n    encoding: b64\n    content: %s\n",
		base64.StdEncoding.EncodeToString([]byte(preload.guestList())))
	fmt.Fprintf(&b, "runcmd:\n  - [bash, /var/lib/eph-build/guest/provision.sh, %q, %q, %q]\n", version, runner.Version, runner.SHA256)
	b.WriteString("power_state:\n  mode: poweroff\n  timeout: 60\n  condition: true\n")
	return b.Bytes(), nil
}

// provisionTail is the end of what provision.sh printed (cloud-init prefixes
// it with "cloud-init["), which is where a failure shows, rather than the
// shutdown that follows it.
func provisionTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(unavailable)"
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.Contains(l, "cloud-init[") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) == 0 {
		return logTail(path)
	}
	return strings.Join(lines[max(0, len(lines)-25):], "\n")
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// runnerRelease is the Actions runner an image gets.
type runnerRelease struct {
	Version string
	SHA256  string // of actions-runner-linux-x64-<version>.tar.gz
}

const runnerReleaseURL = "https://api.github.com/repos/actions/runner/releases/latest"

var runnerSHARE = regexp.MustCompile(`<!-- BEGIN SHA linux-x64 -->([0-9a-f]{64})<!-- END SHA linux-x64 -->`)

// latestRunner finds the newest Actions runner release and its checksum.
// The response is cached with its ETag: revalidating costs nothing against
// GitHub's unauthenticated rate limit (60 requests an hour per IP, shared
// with everything behind the same address), and if GitHub refuses anyway
// the cached release is used.
func latestRunner(ctx context.Context, log *slog.Logger, cacheDir string) (runnerRelease, error) {
	body := filepath.Join(cacheDir, "runner-release.json")
	etagFile := body + ".etag"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runnerReleaseURL, nil)
	if err != nil {
		return runnerRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if etag, err := os.ReadFile(etagFile); err == nil {
		if _, err := os.Stat(body); err == nil {
			req.Header.Set("If-None-Match", strings.TrimSpace(string(etag)))
		}
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	switch {
	case err != nil:
		log.Warn("Couldn't check for a newer Actions runner; using the cached release", slog.Any("error", err))
	case resp.StatusCode == http.StatusOK:
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return runnerRelease{}, err
		}
		if err := os.WriteFile(body, data, 0o644); err != nil {
			return runnerRelease{}, err
		}
		_ = os.WriteFile(etagFile, []byte(resp.Header.Get("ETag")), 0o644)
	case resp.StatusCode == http.StatusNotModified:
		resp.Body.Close()
	default:
		resp.Body.Close()
		log.Warn("GitHub refused the Actions runner release lookup; using the cached release",
			slog.String("status", resp.Status), slog.String("rateLimitReset", resp.Header.Get("X-RateLimit-Reset")))
	}
	data, err := os.ReadFile(body)
	if err != nil {
		return runnerRelease{}, fmt.Errorf("no Actions runner release available: GitHub's API refused the lookup and nothing is cached in %s (wait for its rate limit to reset, or set GITHUB_TOKEN)", cacheDir)
	}
	return parseRunnerRelease(data)
}

func parseRunnerRelease(data []byte) (runnerRelease, error) {
	var rel struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal(data, &rel); err != nil {
		return runnerRelease{}, fmt.Errorf("parse Actions runner release: %w", err)
	}
	version := strings.TrimPrefix(rel.TagName, "v")
	m := runnerSHARE.FindStringSubmatch(rel.Body)
	if version == "" || m == nil {
		return runnerRelease{}, fmt.Errorf("the Actions runner release %q has no linux-x64 SHA-256 in its notes", rel.TagName)
	}
	return runnerRelease{Version: version, SHA256: m[1]}, nil
}
