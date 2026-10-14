package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Preload is what every golden image this host builds comes with, so jobs
// needn't download it each time: preload.toml in the config dir, optional.
// It only saves time. A job whose pins differ downloads what it needs, as
// it would without it, so a list that has fallen behind never breaks one.
type Preload struct {
	// Playwright lists Playwright versions whose browsers are installed in
	// the runner user's cache, where Playwright looks by default, with the
	// system packages they need.
	Playwright []string `toml:"playwright"`
	// PlaywrightBrowsers are the browsers installed for each version.
	PlaywrightBrowsers []string `toml:"playwright_browsers"`
	// DockerImages are pulled into the image's Docker. Each is pinned by
	// digest, so a job that pulls the same reference gets the same image.
	DockerImages []string `toml:"docker_images"`
}

const preloadFile = "preload.toml"

var defaultPlaywrightBrowsers = []string{"chromium", "firefox", "webkit"}

var (
	playwrightVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	browserNameRE       = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	// An image reference pinned by digest, such as
	// postgres:18@sha256:<64 hex digits>: Docker's reference grammar, a
	// little stricter (a lowercase registry host, no IPv6). It also keeps
	// out anything the guest's shell would read as more than one word.
	pinnedImageRE = regexp.MustCompile(`^[a-z0-9]+([._-]+[a-z0-9]+)*(:[0-9]+)?(/[a-z0-9]+([._-]+[a-z0-9]+)*)*` +
		`(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[0-9a-f]{64}$`)
	// Candidates for pinnedImages, which narrows them to pinnedImageRE.
	pinnedImageIn = regexp.MustCompile(`[A-Za-z0-9_][A-Za-z0-9_.:/-]*@sha256:[0-9a-f]{64}`)
	// A Compose variable and its default value operator.
	composeVarPrefix = regexp.MustCompile(`^[A-Za-z0-9_]+:?-`)
)

// LoadPreload reads and checks dir/preload.toml. Without one, nothing is
// preloaded. Only image builds and `check` read it, so a mistake in it
// never stops the controller serving jobs.
func LoadPreload(dir string) (Preload, error) {
	var p Preload
	path := filepath.Join(dir, preloadFile)
	md, err := toml.DecodeFile(path, &p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Preload{}, nil
		}
		return p, fmt.Errorf("read %s: %w", path, err)
	}
	// A misspelt key would otherwise quietly preload nothing.
	if keys := md.Undecoded(); len(keys) > 0 {
		return p, fmt.Errorf("%s: unknown keys %v", path, keys)
	}
	if len(p.PlaywrightBrowsers) == 0 {
		p.PlaywrightBrowsers = defaultPlaywrightBrowsers
	}
	return p, p.Validate()
}

func (p Preload) Validate() error {
	var errs []error
	for _, v := range p.Playwright {
		if !playwrightVersionRE.MatchString(v) {
			errs = append(errs, fmt.Errorf("%s: playwright %q must be a version such as 1.56.0", preloadFile, v))
		}
	}
	for _, b := range p.PlaywrightBrowsers {
		if !browserNameRE.MatchString(b) {
			errs = append(errs, fmt.Errorf("%s: playwright_browsers: %q isn't a browser name such as chromium", preloadFile, b))
		}
	}
	for _, ref := range p.DockerImages {
		if !pinnedImageRE.MatchString(ref) {
			errs = append(errs, fmt.Errorf("%s: docker_images: %q must be pinned by digest, as name@sha256:<64 hex digits>", preloadFile, ref))
		}
	}
	return errors.Join(errs...)
}

// guestList is the preload list as provision.sh reads it: one item per
// line, a kind and its arguments, every word already checked by Validate.
func (p Preload) guestList() string {
	var b strings.Builder
	for _, v := range p.Playwright {
		fmt.Fprintf(&b, "playwright %s %s\n", v, strings.Join(p.PlaywrightBrowsers, " "))
	}
	for _, ref := range p.DockerImages {
		fmt.Fprintf(&b, "docker %s\n", ref)
	}
	return b.String()
}

// writePreloadConfig writes a preload.toml with the pins found in files:
// the Playwright version a package-lock.json installs, and every image
// reference pinned by digest in any of them.
func writePreloadConfig(w io.Writer, files []string) error {
	var p Preload
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if filepath.Base(f) == "package-lock.json" {
			v, err := lockedPlaywright(data)
			if err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			if v != "" && !slices.Contains(p.Playwright, v) {
				p.Playwright = append(p.Playwright, v)
			}
		}
		for _, ref := range pinnedImages(string(data)) {
			if !slices.Contains(p.DockerImages, ref) {
				p.DockerImages = append(p.DockerImages, ref)
			}
		}
	}
	if len(p.Playwright) == 0 && len(p.DockerImages) == 0 {
		return fmt.Errorf("found no Playwright version or image pinned by digest in %s", strings.Join(files, ", "))
	}
	slices.Sort(p.Playwright)
	slices.Sort(p.DockerImages)
	if err := p.Validate(); err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString("# Written by `ephemeral-runners preload-config`. Install it as\n")
	b.WriteString("# /etc/ephemeral-runners/preload.toml; see \"Preloading\" in README.md.\n")
	if len(p.Playwright) > 0 {
		fmt.Fprintf(&b, "playwright = [%s]\n", quoteList(p.Playwright, ", "))
	}
	if len(p.DockerImages) > 0 {
		fmt.Fprintf(&b, "docker_images = [\n  %s,\n]\n", quoteList(p.DockerImages, ",\n  "))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// pinnedImages finds the image references pinned by digest in text,
// leaving out what only looks like one: a URL, the tail of a reference
// built from a variable (${REGISTRY}/app@sha256:...), or a longer hex run.
func pinnedImages(text string) []string {
	var refs []string
	for _, m := range pinnedImageIn.FindAllStringIndex(text, -1) {
		ref := text[m[0]:m[1]]
		if m[0] > 0 && strings.ContainsRune("/}$", rune(text[m[0]-1])) {
			continue
		}
		if m[1] < len(text) && isHexDigit(text[m[1]]) {
			continue
		}
		// docker://image@sha256:... is how a workflow's `uses:` names one.
		ref = strings.TrimPrefix(ref, "docker://")
		if strings.Contains(ref, "://") {
			continue
		}
		// ${IMAGE:-postgres:18@sha256:...} (or IMAGE-) matches from IMAGE;
		// the reference is the default after the operator. The :+ and +
		// forms need nothing: the match starts after the +.
		if m[0] > 0 && text[m[0]-1] == '{' {
			op := composeVarPrefix.FindString(ref)
			if op == "" {
				continue
			}
			ref = ref[len(op):]
		}
		if pinnedImageRE.MatchString(ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f'
}

// lockedPlaywright is the version of playwright-core, which holds the
// browser revisions, that a package-lock.json (v2 or later) installs at the
// top level, or "" if it installs none.
func lockedPlaywright(data []byte) (string, error) {
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return "", fmt.Errorf("parse package-lock.json: %w", err)
	}
	return lock.Packages["node_modules/playwright-core"].Version, nil
}

// quoteList quotes words that Validate has checked, so each is a valid TOML
// basic string as it stands.
func quoteList(words []string, sep string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = `"` + w + `"`
	}
	return strings.Join(quoted, sep)
}

// preloadFailures lists the items the build's console reports as left out
// of the image, from provision.sh's EPH-PRELOAD-FAILED lines.
func preloadFailures(console string) []string {
	data, err := os.ReadFile(console)
	if err != nil {
		return nil
	}
	var failed []string
	for _, line := range strings.Split(string(data), "\n") {
		if item, ok := strings.CutPrefix(strings.TrimSpace(line), "EPH-PRELOAD-FAILED "); ok && !slices.Contains(failed, item) {
			failed = append(failed, item)
		}
	}
	return failed
}
