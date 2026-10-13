package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var testDigest = strings.Repeat("0123456789abcdef", 4)

func TestLoadPreload(t *testing.T) {
	// No preload.toml: nothing preloaded, and the host config still loads.
	h, err := LoadHost(t.TempDir())
	if err != nil || len(h.Preload.Playwright) != 0 || len(h.Preload.DockerImages) != 0 {
		t.Fatalf("without preload.toml: %+v, %v", h.Preload, err)
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "preload.toml"), []byte(`playwright = ["1.56.0"]
docker_images = ["ghcr.io/acme/db:1.2@sha256:`+testDigest+`"]
`), 0o600)
	h, err = LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.Preload.PlaywrightBrowsers, []string{"chromium", "firefox", "webkit"}) {
		t.Errorf("default browsers = %v", h.Preload.PlaywrightBrowsers)
	}
	want := "playwright 1.56.0 chromium firefox webkit\ndocker ghcr.io/acme/db:1.2@sha256:" + testDigest + "\n"
	if got := h.Preload.guestList(); got != want {
		t.Errorf("guestList =\n%s\nwant\n%s", got, want)
	}
}

func TestPreloadRejectsWhatTheGuestCantTrust(t *testing.T) {
	for _, body := range []string{
		`docker_images = ["postgres:18"]`,                     // not pinned: a job could get another image
		`docker_images = ["postgres@sha256:abc"]`,             // not a full digest
		`docker_images = ["x@sha256:` + testDigest + `; rm"]`, // more than one word
		`playwright = ["latest"]`,
		`playwright = ["1.56.0 && reboot"]`,
		`playwright = ["1.56.0"]` + "\n" + `playwright_browsers = ["Chromium"]`,
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "preload.toml"), []byte(body), 0o600)
		if _, err := LoadHost(dir); err == nil || !strings.Contains(err.Error(), "preload.toml") {
			t.Errorf("%s: expected a preload.toml error, got %v", body, err)
		}
	}
	// check (and so install.sh) refuses a bad preload.toml too.
	dir := writeConfigDir(t, "", map[string]string{"acme": deploymentTOML("https://github.com/acme")})
	os.WriteFile(filepath.Join(dir, "preload.toml"), []byte(`docker_images = ["postgres:18"]`), 0o600)
	if _, err := LoadConfig(dir); err == nil || !strings.Contains(err.Error(), "pinned by digest") {
		t.Fatalf("LoadConfig: expected a pinning error, got %v", err)
	}
}

func TestPreloadExample(t *testing.T) {
	dir := t.TempDir()
	example, err := os.ReadFile("preload.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "preload.toml"), example, 0o600)
	if _, err := LoadHost(dir); err != nil {
		t.Fatalf("preload.example.toml: %v", err)
	}
}

func TestPreloadConfig(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "package-lock.json")
	os.WriteFile(lock, []byte(`{"lockfileVersion": 3, "packages": {
		"": {"name": "app"},
		"node_modules/playwright": {"version": "1.56.0"},
		"node_modules/playwright-core": {"version": "1.56.0"},
		"node_modules/other/node_modules/playwright-core": {"version": "1.40.0"}
	}}`), 0o600)
	compose := filepath.Join(dir, "compose.yaml")
	os.WriteFile(compose, []byte(`services:
  db:
    image: postgres:18.1-trixie@sha256:`+testDigest+`
  again:
    image: postgres:18.1-trixie@sha256:`+testDigest+`
  cache:
    image: "ghcr.io/acme/cache@sha256:`+strings.Repeat("ab", 32)+`"
  floating:
    image: redis:8
# https://example.com/manifests/sha256:`+testDigest+`
# https://example.com/x@sha256:`+testDigest+`
`), 0o600)
	script := filepath.Join(dir, "gate.mjs")
	os.WriteFile(script, []byte(`const TOOL = 'registry.example.com:5000/tools/lint:v1@sha256:`+strings.Repeat("cd", 32)+`';`), 0o600)

	var out strings.Builder
	if err := writePreloadConfig(&out, []string{lock, compose, script}); err != nil {
		t.Fatal(err)
	}
	// What it prints must load back as a valid preload.toml.
	cfgDir := t.TempDir()
	os.WriteFile(filepath.Join(cfgDir, "preload.toml"), []byte(out.String()), 0o600)
	h, err := LoadHost(cfgDir)
	if err != nil {
		t.Fatalf("%v in:\n%s", err, out.String())
	}
	if !slices.Equal(h.Preload.Playwright, []string{"1.56.0"}) {
		t.Errorf("playwright = %v", h.Preload.Playwright)
	}
	want := []string{
		"ghcr.io/acme/cache@sha256:" + strings.Repeat("ab", 32),
		"postgres:18.1-trixie@sha256:" + testDigest,
		"registry.example.com:5000/tools/lint:v1@sha256:" + strings.Repeat("cd", 32),
	}
	if !slices.Equal(h.Preload.DockerImages, want) {
		t.Errorf("docker_images =\n%v\nwant\n%v", h.Preload.DockerImages, want)
	}

	empty := filepath.Join(dir, "README.md")
	os.WriteFile(empty, []byte("nothing pinned here\n"), 0o600)
	if err := writePreloadConfig(&out, []string{empty}); err == nil {
		t.Fatal("expected an error when nothing is found")
	}
}

func TestCloudConfigCarriesPreload(t *testing.T) {
	p := Preload{Playwright: []string{"1.56.0"}, PlaywrightBrowsers: []string{"webkit"}}
	data, err := cloudConfig("24.04", runnerRelease{Version: "2.338.0", SHA256: "abc"}, p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "/var/lib/eph-build/guest/preload.sh") {
		t.Error("cloud-config is missing preload.sh")
	}
	m := regexp.MustCompile(`path: /var/lib/eph-build/preload\n.*\n.*\n    content: (\S+)`).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("cloud-config is missing the preload list:\n%s", s)
	}
	list, _ := base64.StdEncoding.DecodeString(m[1])
	if string(list) != "playwright 1.56.0 webkit\n" {
		t.Errorf("preload list = %q", list)
	}
	// provision.sh must read the file the host writes, and run the script.
	provision, _ := guestFiles.ReadFile("images/provision.sh")
	for _, want := range []string{"done </var/lib/eph-build/preload", `"$guest/preload.sh"`, "EPH-PRELOAD-FAILED"} {
		if !strings.Contains(string(provision), want) {
			t.Errorf("provision.sh doesn't contain %q", want)
		}
	}
}

func TestPreloadFailures(t *testing.T) {
	console := filepath.Join(t.TempDir(), "console.log")
	os.WriteFile(console, []byte(strings.Join([]string{
		"[   12.3] cloud-init[812]: + echo 'EPH-PRELOAD-FAILED docker x@sha256:1'",
		"EPH-PRELOAD-FAILED docker x@sha256:1\r",
		"EPH-PRELOAD-OK playwright 1.56.0 chromium",
		"EPH-PRELOAD-FAILED playwright 1.57.0 webkit",
		"EPH-BUILD-OK",
	}, "\n")), 0o600)
	got := preloadFailures(console)
	want := []string{"docker x@sha256:1", "playwright 1.57.0 webkit"}
	if !slices.Equal(got, want) {
		t.Fatalf("preloadFailures = %q, want %q", got, want)
	}
}
