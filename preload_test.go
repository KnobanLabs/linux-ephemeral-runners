package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var testDigest = strings.Repeat("0123456789abcdef", 4)

// testIntegrity is a lockfile's integrity for a tarball whose bytes are s.
func testIntegrity(s string) string {
	sum := sha512.Sum512([]byte(s))
	return "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
}

// testPackage is an npm_packages entry for name's tarball at version.
func testPackage(name, version string) string {
	file := name[strings.LastIndex(name, "/")+1:]
	return "https://registry.npmjs.org/" + name + "/-/" + file + "-" + version + ".tgz " + testIntegrity(name+version)
}

func TestLoadPreload(t *testing.T) {
	// No preload.toml: nothing preloaded.
	p, err := LoadPreload(t.TempDir())
	if err != nil || len(p.Playwright) != 0 || len(p.DockerImages) != 0 {
		t.Fatalf("without preload.toml: %+v, %v", p, err)
	}

	dir := t.TempDir()
	left, scoped := testPackage("left-pad", "1.3.0"), testPackage("@acme/util", "2.0.0")
	os.WriteFile(filepath.Join(dir, "preload.toml"), []byte(`playwright = ["1.56.0"]
docker_images = ["ghcr.io/acme/db:1.2@sha256:`+testDigest+`"]
node = ["24", "22.11.0"]
npm_packages = ["`+left+`", "`+scoped+`", "`+left+`"]
`), 0o600)
	p, err = LoadPreload(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.PlaywrightBrowsers, []string{"chromium", "firefox", "webkit"}) {
		t.Errorf("default browsers = %v", p.PlaywrightBrowsers)
	}
	// The packages file is sorted, without repeats, and the npm item names
	// it by count and hash.
	list := scoped + "\n" + left + "\n"
	if got := p.npmPackageList(); got != list {
		t.Errorf("npmPackageList =\n%s\nwant\n%s", got, list)
	}
	want := "node 24\nnode 22.11.0\n" +
		fmt.Sprintf("npm 2 sha256:%x\n", sha256.Sum256([]byte(list))) +
		"playwright 1.56.0 chromium firefox webkit\ndocker ghcr.io/acme/db:1.2@sha256:" + testDigest + "\n"
	if got := p.guestList(); got != want {
		t.Errorf("guestList =\n%s\nwant\n%s", got, want)
	}

	// The order the packages are listed in doesn't change the item; another
	// package does.
	reordered := Preload{Node: p.Node, NPMPackages: []string{scoped, left}}
	if !strings.Contains(p.guestList(), strings.Split(reordered.guestList(), "\n")[2]) {
		t.Errorf("reordering the packages changed the item: %s", reordered.guestList())
	}
	changed := Preload{Node: p.Node, NPMPackages: []string{scoped, testPackage("left-pad", "1.3.1")}}
	if strings.Contains(p.guestList(), strings.Split(changed.guestList(), "\n")[2]) {
		t.Errorf("changing a package left the item as it was: %s", changed.guestList())
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
		`node = ["lts/*"]`,
		`node = ["24.11"]`, // setup-node would read it as a range
		`node = ["v24"]`,
		`node = ["24; reboot"]`,
		`node = ["24"]` + "\n" + `npm_packages = ["https://registry.example.com/left-pad/-/left-pad-1.3.0.tgz ` + testIntegrity("x") + `"]`,
		`node = ["24"]` + "\n" + `npm_packages = ["http://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz ` + testIntegrity("x") + `"]`,
		`node = ["24"]` + "\n" + `npm_packages = ["https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tar.gz ` + testIntegrity("x") + `"]`,
		`node = ["24"]` + "\n" + `npm_packages = ["https://registry.npmjs.org/left-pad/left-pad-1.3.0.tgz ` + testIntegrity("x") + `"]`,
		`node = ["24"]` + "\n" + `npm_packages = ["https://registry.npmjs.org/left%20pad/-/x-1.3.0.tgz ` + testIntegrity("x") + `"]`,
		`node = ["24"]` + "\n" + `npm_packages = ["https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz sha1-` + strings.Repeat("A", 27) + `="]`,
		`node = ["24"]` + "\n" + `npm_packages = ["https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz sha512-abc=="]`,
		`node = ["24"]` + "\n" + `npm_packages = ["https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz ` + testIntegrity("x") + `; reboot"]`,
		`node = ["24"]` + "\n" + `npm_packages = ["https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz"]`,
		`npm_packages = ["` + testPackage("left-pad", "1.3.0") + `"]`, // no node to fill the cache with
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

	var out, stderr strings.Builder
	if err := writePreloadConfig(&out, &stderr, []string{lock, compose, script, workflow}); err != nil {
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
	if err := writePreloadConfig(&out, &stderr, []string{empty}); err == nil {
		t.Fatal("expected an error when nothing is found")
	}
}

func TestPreloadConfigNodeAndNPM(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "package-lock.json")
	pkg := func(resolved, integrity, extra string) string {
		return fmt.Sprintf(`{"version": "1.0.0", "resolved": %q, "integrity": %q%s}`, resolved, integrity, extra)
	}
	reg := "https://registry.npmjs.org/"
	os.WriteFile(lock, []byte(`{"lockfileVersion": 3, "packages": {
		"": {"name": "app", "dependencies": {"left-pad": "^1.3.0"}},
		"node_modules/left-pad": `+pkg(reg+"left-pad/-/left-pad-1.3.0.tgz", testIntegrity("left-pad"), "")+`,
		"node_modules/other/node_modules/left-pad": `+pkg(reg+"left-pad/-/left-pad-1.3.0.tgz", testIntegrity("left-pad"), "")+`,
		"node_modules/@acme/util": `+pkg(reg+"@acme/util/-/util-2.0.0.tgz", testIntegrity("util"), "")+`,
		"node_modules/pad-alias": `+pkg(reg+"left-pad/-/left-pad-1.1.0.tgz", testIntegrity("alias"), `, "name": "left-pad"`)+`,
		"node_modules/both-hashes": `+pkg(reg+"both-hashes/-/both-hashes-1.0.0.tgz", "sha1-"+strings.Repeat("A", 27)+"= "+testIntegrity("both"), "")+`,
		"node_modules/sha1-only": `+pkg(reg+"sha1-only/-/sha1-only-1.0.0.tgz", "sha1-"+strings.Repeat("A", 27)+"=", "")+`,
		"node_modules/elsewhere": `+pkg("https://npm.example.com/elsewhere/-/elsewhere-1.0.0.tgz", testIntegrity("elsewhere"), "")+`,
		"node_modules/from-git": {"version": "1.0.0", "resolved": "git+ssh://git@example.com/acme/from-git.git#abc"},
		"node_modules/local": {"resolved": "packages/local", "link": true},
		"packages/local": {"name": "local", "version": "1.0.0"},
		"node_modules/parent/node_modules/bundled": `+pkg(reg+"bundled/-/bundled-1.0.0.tgz", testIntegrity("bundled"), `, "inBundle": true`)+`,
		"node_modules/@esbuild/linux-x64": `+pkg(reg+"@esbuild/linux-x64/-/linux-x64-0.25.0.tgz", testIntegrity("lx"), `, "optional": true, "os": ["linux"], "cpu": ["x64"]`)+`,
		"node_modules/@esbuild/darwin-arm64": `+pkg(reg+"@esbuild/darwin-arm64/-/darwin-arm64-0.25.0.tgz", testIntegrity("da"), `, "optional": true, "os": ["darwin"], "cpu": ["arm64"]`)+`,
		"node_modules/@esbuild/linux-arm64": `+pkg(reg+"@esbuild/linux-arm64/-/linux-arm64-0.25.0.tgz", testIntegrity("la"), `, "optional": true, "os": ["linux"], "cpu": ["arm64"]`)+`,
		"node_modules/@rollup/rollup-linux-x64-gnu": `+pkg(reg+"@rollup/rollup-linux-x64-gnu/-/rollup-linux-x64-gnu-4.0.0.tgz", testIntegrity("gnu"), `, "optional": true, "os": ["linux"], "cpu": ["x64"], "libc": ["glibc"]`)+`,
		"node_modules/@rollup/rollup-linux-x64-musl": `+pkg(reg+"@rollup/rollup-linux-x64-musl/-/rollup-linux-x64-musl-4.0.0.tgz", testIntegrity("musl"), `, "optional": true, "os": ["linux"], "cpu": ["x64"], "libc": ["musl"]`)+`,
		"node_modules/not-windows": `+pkg(reg+"not-windows/-/not-windows-1.0.0.tgz", testIntegrity("nw"), `, "os": ["!win32"]`)+`,
		"node_modules/not-linux": `+pkg(reg+"not-linux/-/not-linux-1.0.0.tgz", testIntegrity("nl"), `, "os": "!linux"`)+`,
		"node_modules/playwright-core": `+pkg(reg+"playwright-core/-/playwright-core-1.56.0.tgz", testIntegrity("pw"), `, "version": "1.56.0"`)+`
	}}`), 0o600)
	nvmrc := filepath.Join(dir, ".nvmrc")
	os.WriteFile(nvmrc, []byte("v24\n"), 0o600)

	var out, stderr strings.Builder
	if err := writePreloadConfig(&out, &stderr, []string{lock, nvmrc}); err != nil {
		t.Fatal(err)
	}
	cfgDir := t.TempDir()
	os.WriteFile(filepath.Join(cfgDir, "preload.toml"), []byte(out.String()), 0o600)
	p, err := LoadPreload(cfgDir)
	if err != nil {
		t.Fatalf("%v in:\n%s", err, out.String())
	}
	if !slices.Equal(p.Node, []string{"24"}) || !slices.Equal(p.Playwright, []string{"1.56.0"}) {
		t.Errorf("node = %v, playwright = %v", p.Node, p.Playwright)
	}
	want := []string{
		reg + "@acme/util/-/util-2.0.0.tgz " + testIntegrity("util"),
		reg + "@esbuild/linux-x64/-/linux-x64-0.25.0.tgz " + testIntegrity("lx"),
		reg + "@rollup/rollup-linux-x64-gnu/-/rollup-linux-x64-gnu-4.0.0.tgz " + testIntegrity("gnu"),
		reg + "both-hashes/-/both-hashes-1.0.0.tgz " + testIntegrity("both"),
		reg + "left-pad/-/left-pad-1.1.0.tgz " + testIntegrity("alias"),
		reg + "left-pad/-/left-pad-1.3.0.tgz " + testIntegrity("left-pad"),
		reg + "not-windows/-/not-windows-1.0.0.tgz " + testIntegrity("nw"),
		reg + "playwright-core/-/playwright-core-1.56.0.tgz " + testIntegrity("pw"),
	}
	if !slices.Equal(p.NPMPackages, want) {
		t.Errorf("npm_packages =\n%s\nwant\n%s", strings.Join(p.NPMPackages, "\n"), strings.Join(want, "\n"))
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected note: %s", stderr.String())
	}

	// A .node-version with an exact version works the same way.
	nodeVersion := filepath.Join(dir, ".node-version")
	os.WriteFile(nodeVersion, []byte("22.11.0"), 0o600)
	out.Reset()
	if err := writePreloadConfig(&out, &stderr, []string{nodeVersion}); err != nil || out.String() !=
		"# Written by `ephemeral-runners preload-config`. Install it as\n"+
			"# /etc/ephemeral-runners/preload.toml; see \"Preloading\" in README.md.\n"+
			"node = [\"22.11.0\"]\n" {
		t.Errorf(".node-version: %v\n%s", err, out.String())
	}

	// Without a Node.js version, what it printed before, and a note.
	out.Reset()
	if err := writePreloadConfig(&out, &stderr, []string{lock}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "npm_packages") || !strings.Contains(out.String(), `playwright = ["1.56.0"]`) {
		t.Errorf("without a Node.js version:\n%s", out.String())
	}
	if !strings.Contains(stderr.String(), ".nvmrc") {
		t.Errorf("no note about the left-out packages: %q", stderr.String())
	}

	// A version setup-node would resolve some other way is set by hand.
	for _, v := range []string{"lts/*", ">=24", "24.11", "node"} {
		os.WriteFile(nvmrc, []byte(v+"\n"), 0o600)
		if err := writePreloadConfig(&out, &stderr, []string{lock, nvmrc}); err == nil || !strings.Contains(err.Error(), "by hand") {
			t.Errorf(".nvmrc %q: got %v", v, err)
		}
	}
}

func TestPlatformAllows(t *testing.T) {
	for _, c := range []struct {
		list []string
		want bool
	}{
		{nil, true},
		{[]string{"any"}, true},
		{[]string{"linux"}, true},
		{[]string{"darwin", "linux"}, true},
		{[]string{"darwin"}, false},
		{[]string{"!win32"}, true},
		{[]string{"!win32", "!linux"}, false},
		{[]string{"!linux", "linux"}, false},
		{[]string{"!win32", "darwin"}, false},
	} {
		if got := platformAllows(c.list, "linux"); got != c.want {
			t.Errorf("platformAllows(%q, linux) = %v", c.list, got)
		}
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
	if strings.Contains(s, npmPackagesGuestPath) {
		t.Error("cloud-config writes an npm package list when there are no packages")
	}

	// The npm item's packages go in a file of their own, which preload.sh
	// reads and checks against the item's hash.
	p = Preload{Node: []string{"24"}, NPMPackages: []string{testPackage("left-pad", "1.3.0")}}
	data, err = cloudConfig("24.04", runnerRelease{Version: "2.338.0", SHA256: "abc"}, p)
	if err != nil {
		t.Fatal(err)
	}
	m = regexp.MustCompile(`path: ` + npmPackagesGuestPath + `\n.*\n.*\n    content: (\S+)`).FindStringSubmatch(string(data))
	if m == nil {
		t.Fatalf("cloud-config is missing the npm package list:\n%s", data)
	}
	gz, _ := base64.StdEncoding.DecodeString(m[1])
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	if list, err := io.ReadAll(zr); err != nil || string(list) != p.npmPackageList() {
		t.Errorf("npm package list = %q, %v", list, err)
	}
	if !strings.Contains(string(data), "path: "+npmPackagesGuestPath+"\n    permissions: '0644'\n    encoding: gz+b64\n") {
		t.Error("the npm package list isn't marked gz+b64 for cloud-init")
	}
	// provision.sh must read the file the host writes, and run the script.
	provision, _ := guestFiles.ReadFile("images/provision.sh")
	for _, want := range []string{"done </var/lib/eph-build/preload", `"$guest/preload.sh"`, "EPH-PRELOAD-FAILED"} {
		if !strings.Contains(string(provision), want) {
			t.Errorf("provision.sh doesn't contain %q", want)
		}
	}
	script, _ := guestFiles.ReadFile("images/preload.sh")
	if !strings.Contains(string(script), "list="+npmPackagesGuestPath+"\n") {
		t.Errorf("preload.sh doesn't read the npm package list from %s", npmPackagesGuestPath)
	}
	// Each version's install must leave earlier versions' browsers alone.
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
		Node:               []string{"24"},
		NPMPackages:        []string{testPackage("left-pad", "1.3.0")},
		Playwright:         []string{"1.56.0", "1.57.0"},
		PlaywrightBrowsers: []string{"chromium"},
		DockerImages:       []string{"postgres@sha256:" + testDigest},
	}
	npm := preloadItems(p.guestList())[1]
	if !strings.HasPrefix(npm, "npm 1 sha256:") {
		t.Fatalf("npm item = %q", npm)
	}
	got := preloadRecord(p, []string{"playwright 1.57.0 chromium", npm})
	want := "node 24\nplaywright 1.56.0 chromium\ndocker postgres@sha256:" + testDigest + "\n"
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
