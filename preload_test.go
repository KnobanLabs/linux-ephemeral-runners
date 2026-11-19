package main

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var testDigest = strings.Repeat("0123456789abcdef", 4)

func TestLoadPreload(t *testing.T) {
	// No preload.toml: nothing preloaded.
	p, err := LoadPreload(t.TempDir())
	if err != nil || len(p.Playwright) != 0 || len(p.DockerImages) != 0 {
		t.Fatalf("without preload.toml: %+v, %v", p, err)
	}

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "preload.toml"), []byte(`playwright = ["1.56.0"]
docker_images = ["ghcr.io/acme/db:1.2@sha256:`+testDigest+`"]
`), 0o600)
	p, err = LoadPreload(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.PlaywrightBrowsers, []string{"chromium", "firefox", "webkit"}) {
		t.Errorf("default browsers = %v", p.PlaywrightBrowsers)
	}
	want := "playwright 1.56.0 chromium firefox webkit\ndocker ghcr.io/acme/db:1.2@sha256:" + testDigest + "\n"
	if got := p.guestList(); got != want {
		t.Errorf("guestList =\n%s\nwant\n%s", got, want)
	}
}

func TestPreloadRejectsWhatTheGuestCantTrust(t *testing.T) {
	for _, body := range []string{
		`docker_images = ["postgres:18"]`,                        // not pinned: a job could get another image
		`docker_images = ["postgres@sha256:abc"]`,                // not a full digest
		`docker_images = ["x@sha256:` + testDigest + `; rm"]`,    // more than one word
		`docker_images = ["Acme/App@sha256:` + testDigest + `"]`, // Docker rejects uppercase names
		`playwright = ["latest"]`,
		`playwright = ["1.56.0 && reboot"]`,
		`playwright = ["1.56.0"]` + "\n" + `playwright_browsers = ["Chromium"]`,
		`docker_image = ["x@sha256:` + testDigest + `"]`, // misspelt: would preload nothing
	} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "preload.toml"), []byte(body), 0o600)
		if _, err := LoadPreload(dir); err == nil || !strings.Contains(err.Error(), "preload.toml") {
			t.Errorf("%s: expected a preload.toml error, got %v", body, err)
		}
	}
}

func TestBadPreloadNeverStopsTheController(t *testing.T) {
	dir := writeConfigDir(t, "", map[string]string{"acme": deploymentTOML("https://github.com/acme")})
	os.WriteFile(filepath.Join(dir, "preload.toml"), []byte(`docker_images = ["postgres:18"]`), 0o600)
	if _, err := LoadConfig(dir); err != nil {
		t.Fatalf("LoadConfig, which run uses, must not read preload.toml: %v", err)
	}
}

func TestPreloadExample(t *testing.T) {
	dir := t.TempDir()
	example, err := os.ReadFile("preload.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "preload.toml"), example, 0o600)
	if _, err := LoadPreload(dir); err != nil {
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
  defaulted:
    image: ${KV_IMAGE:-valkey/valkey:9.0-trixie@sha256:`+strings.Repeat("12", 32)+`}
  underscored:
    image: ghcr.io/acme/data_loader:1.0_slim@sha256:`+strings.Repeat("34", 32)+`
  lowercase-default:
    image: ${db_image:-postgres:18@sha256:`+strings.Repeat("56", 32)+`}
  unset-default:
    image: ${pg_image-postgres:17@sha256:`+strings.Repeat("78", 32)+`}
  unset-default-upper:
    image: ${PG_IMAGE-postgres:16@sha256:`+strings.Repeat("9a", 32)+`}
  underscore-variable:
    image: ${_PG:-postgres:15@sha256:`+strings.Repeat("bc", 32)+`}
  alternate:
    image: ${USE_PG:+postgres:14@sha256:`+strings.Repeat("de", 32)+`}
# https://example.com/manifests/sha256:`+testDigest+`
# https://example.com/x@sha256:`+testDigest+`
`), 0o600)
	script := filepath.Join(dir, "gate.mjs")
	os.WriteFile(script, []byte(`const TOOL = 'registry.example.com:5000/tools/lint:v1@sha256:`+strings.Repeat("cd", 32)+`';`), 0o600)
	workflow := filepath.Join(dir, "ci.yaml")
	os.WriteFile(workflow, []byte(`steps:
  - uses: docker://ghcr.io/acme/action@sha256:`+strings.Repeat("ef", 32)+`
  - run: docker run ${REGISTRY}/tools/built@sha256:`+testDigest+`
  - run: echo x@sha256:`+testDigest+`ff
`), 0o600)

	var out strings.Builder
	if err := writePreloadConfig(&out, []string{lock, compose, script, workflow}); err != nil {
		t.Fatal(err)
	}
	// What it prints must load back as a valid preload.toml.
	cfgDir := t.TempDir()
	os.WriteFile(filepath.Join(cfgDir, "preload.toml"), []byte(out.String()), 0o600)
	p, err := LoadPreload(cfgDir)
	if err != nil {
		t.Fatalf("%v in:\n%s", err, out.String())
	}
	if !slices.Equal(p.Playwright, []string{"1.56.0"}) {
		t.Errorf("playwright = %v", p.Playwright)
	}
	want := []string{
		"ghcr.io/acme/action@sha256:" + strings.Repeat("ef", 32),
		"ghcr.io/acme/cache@sha256:" + strings.Repeat("ab", 32),
		"ghcr.io/acme/data_loader:1.0_slim@sha256:" + strings.Repeat("34", 32),
		"postgres:14@sha256:" + strings.Repeat("de", 32),
		"postgres:15@sha256:" + strings.Repeat("bc", 32),
		"postgres:16@sha256:" + strings.Repeat("9a", 32),
		"postgres:17@sha256:" + strings.Repeat("78", 32),
		"postgres:18.1-trixie@sha256:" + testDigest,
		"postgres:18@sha256:" + strings.Repeat("56", 32),
		"registry.example.com:5000/tools/lint:v1@sha256:" + strings.Repeat("cd", 32),
		"valkey/valkey:9.0-trixie@sha256:" + strings.Repeat("12", 32),
	}
	if !slices.Equal(p.DockerImages, want) {
		t.Errorf("docker_images =\n%v\nwant\n%v", p.DockerImages, want)
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
	// Each version's install must leave earlier versions' browsers alone.
	script, _ := guestFiles.ReadFile("images/preload.sh")
	if !strings.Contains(string(script), "PLAYWRIGHT_SKIP_BROWSER_GC=1") {
		t.Error("preload.sh lets Playwright delete the browsers of versions installed before")
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

func TestPreloadRecord(t *testing.T) {
	p := Preload{
		Playwright:         []string{"1.56.0", "1.57.0"},
		PlaywrightBrowsers: []string{"chromium"},
		DockerImages:       []string{"postgres@sha256:" + testDigest},
	}
	got := preloadRecord(p, []string{"playwright 1.57.0 chromium"})
	want := "playwright 1.56.0 chromium\ndocker postgres@sha256:" + testDigest + "\n"
	if got != want {
		t.Errorf("preloadRecord =\n%s\nwant\n%s", got, want)
	}
	if got := preloadRecord(Preload{}, nil); got != "" {
		t.Errorf("preloadRecord of nothing = %q", got)
	}
}

// preloadWarnings runs checkPreloadRecords against images whose records
// are given, and returns the images it warns about with their messages.
func preloadWarnings(t *testing.T, p Preload, records map[string]string) map[string]string {
	t.Helper()
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Host: HostConfig{StateDir: state},
		Deployments: []*Deployment{{Pools: []Pool{
			{Image: "ubuntu-24.04"}, {Image: "ubuntu-26.04"}, {Image: "custom"},
		}}},
	}
	for name, body := range records {
		if err := os.WriteFile(cfg.Host.PreloadRecordPath(name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	checkPreloadRecords(slog.New(slog.NewTextHandler(&out, nil)), cfg, p)
	warned := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if m := regexp.MustCompile(`image=(\S+)`).FindStringSubmatch(line); m != nil && strings.Contains(line, "level=WARN") {
			warned[m[1]] = line
		}
	}
	return warned
}

func TestCheckPreloadRecords(t *testing.T) {
	p := Preload{Playwright: []string{"1.56.0"}, PlaywrightBrowsers: defaultPlaywrightBrowsers}
	current := preloadRecord(p, nil)

	// Nothing to preload, and images built without records or with empty
	// ones: nothing to say.
	if w := preloadWarnings(t, Preload{}, map[string]string{"ubuntu-24.04": ""}); len(w) != 0 {
		t.Errorf("nothing preloaded, warned %v", w)
	}

	// A host that gained a preload.toml after its images were built.
	w := preloadWarnings(t, p, map[string]string{"ubuntu-24.04": current})
	if _, ok := w["ubuntu-24.04"]; ok {
		t.Errorf("warned about an image that preloads the list: %s", w["ubuntu-24.04"])
	}
	if !strings.Contains(w["ubuntu-26.04"], `missing="[playwright 1.56.0 chromium firefox webkit]"`) ||
		!strings.Contains(w["ubuntu-26.04"], "ephemeral-runners-images") {
		t.Errorf("an image without a record: %q", w["ubuntu-26.04"])
	}
	if _, ok := w["custom"]; ok {
		t.Error("warned about a custom image, which build-image doesn't make")
	}

	// The list changed, or a build left an item out: both differ.
	w = preloadWarnings(t, Preload{}, map[string]string{"ubuntu-24.04": current, "ubuntu-26.04": ""})
	if !strings.Contains(w["ubuntu-24.04"], "extra=") || len(w) != 1 {
		t.Errorf("an image preloading what the list dropped: %v", w)
	}
	w = preloadWarnings(t, p, map[string]string{"ubuntu-24.04": current, "ubuntu-26.04": preloadRecord(p, preloadItems(current))})
	if len(w) != 1 || w["ubuntu-26.04"] == "" {
		t.Errorf("an image whose build left an item out: %v", w)
	}
}
