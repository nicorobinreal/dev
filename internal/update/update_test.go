package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicorobinreal/dev/internal/state"
)

func makeTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{"README.md", []byte("readme")}, {name, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeRelease struct {
	tag       string
	archive   []byte
	checksums string
	hits      map[string]int
}

func newFake(t *testing.T, tag string, bin []byte) (*fakeRelease, *httptest.Server) {
	t.Helper()
	f := &fakeRelease{tag: tag, archive: makeTarGz(t, "dev", bin), hits: map[string]int{}}
	sum := sha256.Sum256(f.archive)
	f.checksums = fmt.Sprintf("%x  %s\n%x  dev_linux_amd64.tar.gz\n", sum, AssetName("darwin", "arm64"), sha256.Sum256([]byte("x")))
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits[r.URL.Path]++
		switch r.URL.Path {
		case "/repos/nicorobinreal/dev/releases/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": f.tag,
				"assets": []map[string]string{
					{"name": AssetName("darwin", "arm64"), "browser_download_url": srv.URL + "/dl/archive"},
					{"name": ChecksumsAsset, "browser_download_url": srv.URL + "/dl/checksums"},
				},
			})
		case "/dl/archive":
			_, _ = w.Write(f.archive)
		case "/dl/checksums":
			_, _ = w.Write([]byte(f.checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func testClient(t *testing.T, srv *httptest.Server) (*Client, string) {
	t.Helper()
	home := t.TempDir()
	c := New(state.Paths{Home: home})
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()
	c.GOOS, c.GOARCH = "darwin", "arm64"
	c.Now = func() time.Time { return time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC) }
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, "dev")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return c, exe
}

func TestUpdateSwapsAtomically(t *testing.T) {
	_, srv := newFake(t, "v1.2.0", []byte("new binary"))
	c, exe := testClient(t, srv)

	from, to, updated, err := c.Update(context.Background(), "v1.1.0", exe)
	if err != nil {
		t.Fatal(err)
	}
	if !updated || from != "v1.1.0" || to != "v1.2.0" {
		t.Fatalf("got %s→%s updated=%v", from, to, updated)
	}
	b, _ := os.ReadFile(exe)
	if string(b) != "new binary" {
		t.Fatalf("binary = %q", b)
	}
	fi, _ := os.Stat(exe)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	ents, _ := os.ReadDir(filepath.Dir(exe))
	if len(ents) != 1 {
		t.Fatalf("leftover temp files: %v", ents)
	}
	ca := LoadCache(c.Paths)
	if ca.LastSeen != "v1.2.0" || !ca.LastCheck.Equal(c.Now()) {
		t.Fatalf("cache = %+v", ca)
	}

	// Notice prints once.
	n, err := TakeNotice(c.Paths)
	if err != nil || n != "dev updated v1.1.0 → v1.2.0" {
		t.Fatalf("notice = %q, %v", n, err)
	}
	if n, _ := TakeNotice(c.Paths); n != "" {
		t.Fatalf("second notice = %q", n)
	}
}

func TestUpdateUpToDate(t *testing.T) {
	f, srv := newFake(t, "v1.2.0", []byte("new binary"))
	c, exe := testClient(t, srv)
	for _, cur := range []string{"v1.2.0", "v1.3.0"} {
		_, _, updated, err := c.Update(context.Background(), cur, exe)
		if err != nil || updated {
			t.Fatalf("%s: updated=%v err=%v", cur, updated, err)
		}
	}
	if f.hits["/dl/archive"] != 0 {
		t.Fatal("downloaded archive while up to date")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("binary replaced")
	}
	if n, _ := TakeNotice(c.Paths); n != "" {
		t.Fatalf("notice = %q", n)
	}
}

func TestUpdateFromDevBuild(t *testing.T) {
	_, srv := newFake(t, "v0.3.0", []byte("new binary"))
	c, exe := testClient(t, srv)
	_, _, updated, err := c.Update(context.Background(), "dev", exe)
	if err != nil || !updated {
		t.Fatalf("updated=%v err=%v", updated, err)
	}
}

func TestChecksumMismatchRejected(t *testing.T) {
	f, srv := newFake(t, "v1.2.0", []byte("new binary"))
	f.checksums = strings.Repeat("0", 64) + "  " + AssetName("darwin", "arm64") + "\n"
	c, exe := testClient(t, srv)
	_, _, updated, err := c.Update(context.Background(), "v1.1.0", exe)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || updated {
		t.Fatalf("updated=%v err=%v", updated, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Fatal("binary replaced despite mismatch")
	}
	if ents, _ := os.ReadDir(filepath.Dir(exe)); len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
	if n, _ := TakeNotice(c.Paths); n != "" {
		t.Fatalf("notice = %q", n)
	}
}

func TestMissingAsset(t *testing.T) {
	_, srv := newFake(t, "v1.2.0", []byte("new binary"))
	c, exe := testClient(t, srv)
	c.GOARCH = "riscv64"
	_, _, _, err := c.Update(context.Background(), "v1.1.0", exe)
	if err == nil || !strings.Contains(err.Error(), "no asset") {
		t.Fatalf("err = %v", err)
	}
}

func TestShouldCheck(t *testing.T) {
	p := state.Paths{Home: t.TempDir()}
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	if !ShouldCheck(p, now) {
		t.Fatal("no cache: should check")
	}
	if err := MarkChecked(p, now); err != nil {
		t.Fatal(err)
	}
	if ShouldCheck(p, now.Add(59*time.Minute)) {
		t.Fatal("59m: should not check")
	}
	if !ShouldCheck(p, now.Add(time.Hour)) {
		t.Fatal("1h: should check")
	}
	if !ShouldCheck(p, now.Add(-2*time.Hour)) {
		t.Fatal("last check in the future: should check")
	}
}

func TestMarkCheckedKeepsNotice(t *testing.T) {
	p := state.Paths{Home: t.TempDir()}
	if _, err := modifyCache(p, func(c *Cache) { c.Notice = &Notice{From: "v1.0.0", To: "v1.1.0"} }); err != nil {
		t.Fatal(err)
	}
	if err := MarkChecked(p, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, _ := TakeNotice(p); n != "dev updated v1.0.0 → v1.1.0" {
		t.Fatalf("notice = %q", n)
	}
}

func TestAutoEnabled(t *testing.T) {
	t.Setenv("DEV_NO_UPDATE", "")
	if !AutoEnabled("v1.0.0") {
		t.Fatal("release should auto-update")
	}
	for _, v := range []string{"dev", "", "1.0.0", "v0.1.0-SNAPSHOT-abc"} {
		if AutoEnabled(v) {
			t.Fatalf("%q should not auto-update", v)
		}
	}
	t.Setenv("DEV_NO_UPDATE", "1")
	if AutoEnabled("v1.0.0") {
		t.Fatal("DEV_NO_UPDATE=1 should disable")
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		cand, cur string
		want      bool
	}{
		{"v1.2.0", "v1.1.9", true},
		{"v1.10.0", "v1.9.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.2.0", "v2.0.0", false},
		{"v1.2.0", "v1.2.0-rc1", true},
		{"v1.2.0-rc1", "v1.2.0", false},
		{"v1.2.0", "dev", true},
		{"garbage", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.cand, c.cur); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.cand, c.cur, got)
		}
	}
}
