// Package update checks GitHub releases, downloads and verifies the release
// archive for this platform, and swaps the running binary atomically.
//
// Flow:
//   - Each invocation (command layer): if AutoEnabled(version) and
//     ShouldCheck(paths, now), call Spawn, which records last_check and
//     starts a detached `dev self-update --background`.
//   - That subprocess (and an explicit `dev self-update`) calls
//     Client.Update under the global lock.
//   - The next invocation calls TakeNotice and prints "dev updated vX → vY".
package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/nicorobinreal/dev/internal/state"
)

const (
	// DefaultBaseURL is the GitHub API root.
	DefaultBaseURL = "https://api.github.com"
	// DefaultRepo is the owner/name of the release repository.
	DefaultRepo = "nicorobinreal/dev"
	// ChecksumsAsset is the GoReleaser checksum file name.
	ChecksumsAsset = "checksums.txt"

	maxBinarySize   = 256 << 20 // refuse absurdly large archives/binaries
	maxMetaSize     = 4 << 20   // API JSON and checksums.txt
	binaryInTarball = "dev"
)

// AssetName is the GoReleaser archive name for a platform:
// dev_<goos>_<goarch>.tar.gz (see .goreleaser.yaml).
func AssetName(goos, goarch string) string {
	return fmt.Sprintf("dev_%s_%s.tar.gz", goos, goarch)
}

// Asset is one downloadable file of a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Release is the subset of the GitHub release object we use.
type Release struct {
	Tag    string  `json:"tag_name"`
	Assets []Asset `json:"assets"`
}

func (r Release) asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Client talks to the releases API and performs updates. The zero value is
// not usable; use New.
type Client struct {
	Paths   state.Paths
	BaseURL string // API root; tests point it at httptest
	Repo    string // owner/name
	HTTP    *http.Client
	GOOS    string
	GOARCH  string
	Now     func() time.Time
}

// New returns a Client for the real GitHub API and the running platform.
func New(p state.Paths) *Client {
	return &Client{
		Paths:   p,
		BaseURL: DefaultBaseURL,
		Repo:    DefaultRepo,
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Now:     time.Now,
	}
}

func (c *Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "dev-cli")
	if strings.HasPrefix(url, c.BaseURL) {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", url, limit)
	}
	return b, nil
}

// Latest fetches the latest (non-prerelease) release.
func (c *Client) Latest(ctx context.Context) (Release, error) {
	url := strings.TrimRight(c.BaseURL, "/") + "/repos/" + c.Repo + "/releases/latest"
	b, err := c.get(ctx, url, maxMetaSize)
	if err != nil {
		return Release{}, err
	}
	var r Release
	if err := json.Unmarshal(b, &r); err != nil {
		return Release{}, fmt.Errorf("parse release: %w", err)
	}
	if _, ok := parseVersion(r.Tag); !ok {
		return Release{}, fmt.Errorf("latest release has non-semver tag %q", r.Tag)
	}
	return r, nil
}

// Update checks the latest release and, if it is newer than current (or
// current is not a version at all, e.g. "dev"), downloads the platform
// archive, verifies it against checksums.txt, and atomically replaces
// exePath. It holds the global lock throughout, records last_check and
// last_seen, and on a swap records the pending notice for TakeNotice.
//
// It returns the version transition and whether a swap happened. When
// already up to date it returns (current, latest, false, nil).
func (c *Client) Update(ctx context.Context, current, exePath string) (from, to string, updated bool, err error) {
	lock, err := state.LockGlobal(c.Paths)
	if err != nil {
		return current, "", false, err
	}
	defer lock.Unlock()

	rel, err := c.Latest(ctx)
	now := c.Now()
	if _, cerr := modifyCache(c.Paths, func(ca *Cache) {
		ca.LastCheck = now
		if err == nil {
			ca.LastSeen = rel.Tag
		}
	}); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return current, "", false, err
	}
	if !Newer(rel.Tag, current) {
		return current, rel.Tag, false, nil
	}

	bin, err := c.download(ctx, rel)
	if err != nil {
		return current, rel.Tag, false, err
	}
	if err := swap(exePath, bin); err != nil {
		return current, rel.Tag, false, err
	}
	if _, err := modifyCache(c.Paths, func(ca *Cache) {
		ca.Notice = &Notice{From: current, To: rel.Tag}
	}); err != nil {
		return current, rel.Tag, true, err
	}
	return current, rel.Tag, true, nil
}

// download fetches and verifies the platform archive and returns the
// extracted dev binary.
func (c *Client) download(ctx context.Context, rel Release) ([]byte, error) {
	name := AssetName(c.GOOS, c.GOARCH)
	arch, ok := rel.asset(name)
	if !ok {
		return nil, fmt.Errorf("release %s has no asset %s", rel.Tag, name)
	}
	sums, ok := rel.asset(ChecksumsAsset)
	if !ok {
		return nil, fmt.Errorf("release %s has no %s", rel.Tag, ChecksumsAsset)
	}
	sumsBody, err := c.get(ctx, sums.URL, maxMetaSize)
	if err != nil {
		return nil, err
	}
	want, err := findChecksum(string(sumsBody), name)
	if err != nil {
		return nil, err
	}
	archive, err := c.get(ctx, arch.URL, maxBinarySize)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("checksum mismatch for %s: got %x, want %s", name, got, want)
	}
	return extractBinary(archive)
}

// findChecksum looks up name in a sha256sum-format file ("<hex>  <name>").
func findChecksum(sums, name string) (string, error) {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			sum := strings.ToLower(f[0])
			if len(sum) != 64 {
				return "", fmt.Errorf("malformed checksum for %s", name)
			}
			return sum, nil
		}
	}
	return "", fmt.Errorf("%s not listed in %s", name, ChecksumsAsset)
}

// extractBinary returns the regular file named "dev" from a .tar.gz.
func extractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("archive has no %q binary", binaryInTarball)
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != binaryInTarball {
			continue
		}
		if h.Size <= 0 || h.Size > maxBinarySize {
			return nil, fmt.Errorf("binary in archive has bad size %d", h.Size)
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxBinarySize))
		if err != nil {
			return nil, fmt.Errorf("read binary: %w", err)
		}
		return b, nil
	}
}

// swap writes bin to a temp file next to exePath, chmods it 0755, fsyncs and
// renames it over exePath (atomic on the same filesystem). The running
// process keeps its old inode, so the current run is unaffected.
func swap(exePath string, bin []byte) error {
	dir := filepath.Dir(exePath)
	f, err := os.CreateTemp(dir, ".dev-update-*")
	if err != nil {
		return fmt.Errorf("cannot write next to %s: %w", exePath, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(bin); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, exePath)
}

// Executable returns the resolved path of the running binary (symlinks
// followed), which is what Update should replace.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}

// Spawn records last_check = now and then starts a detached
// `<exe> self-update --background` in its own session with stdio on
// /dev/null. It does not wait: the child outlives this invocation. Recording
// first means concurrent invocations see a fresh check and don't all spawn.
func Spawn(p state.Paths, now time.Time) error {
	if err := MarkChecked(p, now); err != nil {
		return err
	}
	exe, err := Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, "self-update", "--background")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil // os/exec uses /dev/null
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
