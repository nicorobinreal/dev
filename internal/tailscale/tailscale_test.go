package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicorobinreal/dev/internal/run"
)

func TestKeychainGet(t *testing.T) {
	f := (&run.Fake{}).
		On("/usr/bin/security find-generic-password -s dev-tailscale -a client-id -w", "cid-123\n").
		On("/usr/bin/security find-generic-password -s dev-tailscale -a client-secret -w", "sec-456\n")
	cr, err := Keychain{R: f}.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cr.ClientID != "cid-123" || cr.ClientSecret != "sec-456" {
		t.Fatalf("got %+v", cr)
	}
}

func TestKeychainMissing(t *testing.T) {
	f := (&run.Fake{}).Fail("/usr/bin/security find-generic-password", 44,
		"security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.")
	k := Keychain{R: f}
	if _, err := k.Get(context.Background(), AccountClientID); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("want ErrNoCredentials, got %v", err)
	}
	ok, err := k.Has(context.Background())
	if ok || err != nil {
		t.Fatalf("Has = %v, %v", ok, err)
	}
	f2 := (&run.Fake{}).Fail("/usr/bin/security", 51, "boom")
	if _, err := (Keychain{R: f2}).Get(context.Background(), AccountClientID); err == nil || errors.Is(err, ErrNoCredentials) {
		t.Fatalf("want other error, got %v", err)
	}
}

func TestKeychainSet(t *testing.T) {
	f := &run.Fake{}
	k := Keychain{R: f}
	if err := k.Set(context.Background(), AccountClientID, "cid"); err != nil {
		t.Fatal(err)
	}
	if err := k.SetInteractive(context.Background(), AccountClientSecret); err != nil {
		t.Fatal(err)
	}
	lines := f.Lines()
	want := []string{
		"/usr/bin/security add-generic-password -U -s dev-tailscale -a client-id -w cid",
		"/usr/bin/security add-generic-password -U -s dev-tailscale -a client-secret -w",
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, lines[i], want[i])
		}
	}
	if !f.Calls[1].Interactive {
		t.Error("SetInteractive must be interactive")
	}
	if err := k.Set(context.Background(), AccountClientID, ""); err == nil {
		t.Error("empty value accepted")
	}
}

// fakeAPI is a minimal Tailscale API.
type fakeAPI struct {
	t        *testing.T
	tokens   atomic.Int32
	devices  []map[string]any
	deleted  []string
	lastKey  map[string]any
	tokenTTL int
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			f.t.Fatal(err)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			f.t.Errorf("content-type %q", ct)
		}
		if r.PostForm.Get("client_id") != "cid" || r.PostForm.Get("client_secret") != "sec" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"message":"invalid client"}`)
			return
		}
		f.tokens.Add(1)
		ttl := f.tokenTTL
		if ttl == 0 {
			ttl = 3600
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": ttl, "scope": "devices:core auth_keys"})
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /api/v2/tailnet/-/keys", auth(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&f.lastKey)
		io.WriteString(w, `{"id":"k1","key":"tskey-auth-k1-secret","expires":"2026-11-12T00:10:00Z"}`)
	}))
	mux.HandleFunc("GET /api/v2/tailnet/-/devices", auth(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"devices": f.devices})
	}))
	mux.HandleFunc("DELETE /api/v2/device/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "missing" {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"not found"}`)
			return
		}
		f.deleted = append(f.deleted, id)
	}))
	return mux
}

func newTestClient(t *testing.T, f *fakeAPI) *Client {
	f.t = t
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), BaseURL: srv.URL, Creds: StaticCredentials(Credentials{"cid", "sec"})}
}

func TestTokenCached(t *testing.T) {
	f := &fakeAPI{}
	c := newTestClient(t, f)
	ctx := context.Background()
	if err := c.CheckToken(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListDevices(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.tokens.Load(); n != 1 {
		t.Fatalf("minted %d tokens, want 1", n)
	}
	// Expiry forces a refresh.
	c.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := c.CheckToken(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.tokens.Load(); n != 2 {
		t.Fatalf("minted %d tokens, want 2", n)
	}
}

func TestTokenBadCredentials(t *testing.T) {
	f := &fakeAPI{}
	c := newTestClient(t, f)
	c.Creds = StaticCredentials(Credentials{"cid", "wrong"})
	err := c.CheckToken(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || ae.Message != "invalid client" {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatal("secret leaked into error")
	}
}

func TestMintAuthKey(t *testing.T) {
	f := &fakeAPI{}
	c := newTestClient(t, f)
	key, err := c.MintAuthKey(context.Background(), AuthKeyOptions{Tag: "tag:devvm", Hostname: "black-shop"})
	if err != nil {
		t.Fatal(err)
	}
	if key != "tskey-auth-k1-secret" {
		t.Fatalf("key %q", key)
	}
	got, _ := json.Marshal(f.lastKey)
	want := `{"capabilities":{"devices":{"create":{"ephemeral":false,"preauthorized":true,"reusable":false,"tags":["tag:devvm"]}}},"description":"dev black-shop","expirySeconds":600}`
	if string(got) != want {
		t.Fatalf("request\n got %s\nwant %s", got, want)
	}
}

func TestKeyDescription(t *testing.T) {
	long := strings.Repeat("a", 70)
	if d := keyDescription(long); len(d) != 50 || !strings.HasPrefix(d, "dev aaa") {
		t.Fatalf("%q", d)
	}
	if d := keyDescription("x.y"); d != "dev x-y" {
		t.Fatalf("%q", d)
	}
}

func devicesFixture() []map[string]any {
	return []map[string]any{
		{"id": "1", "nodeId": "nMac", "name": "black.tail.ts.net", "hostname": "black", "tags": nil,
			"lastSeen": "2026-11-12T03:25:51Z", "created": "2026-11-09T18:33:04Z", "connectedToControl": true},
		{"id": "2", "nodeId": "nShop", "name": "black-shop.tail.ts.net", "hostname": "black-shop", "tags": []string{"tag:devvm"},
			"lastSeen": "2026-11-12T03:00:00Z", "created": "2026-11-01T00:00:00Z", "connectedToControl": false},
		{"id": "3", "nodeId": "nOld", "name": "black-old.tail.ts.net", "hostname": "ubuntu", "tags": []string{"tag:devvm"},
			"lastSeen": "2026-10-01T00:00:00Z", "created": "2026-09-01T00:00:00Z"},
		{"id": "4", "nodeId": "nFresh", "name": "black-fresh.tail.ts.net", "hostname": "black-fresh", "tags": []string{"tag:devvm"},
			"lastSeen": "2026-11-11T00:00:00Z", "created": "2026-11-01T00:00:00Z"},
		{"id": "5", "nodeId": "nOther", "name": "mbp-old.tail.ts.net", "hostname": "mbp-old", "tags": []string{"tag:devvm"},
			"lastSeen": "2026-10-01T00:00:00Z", "created": "2026-09-01T00:00:00Z"},
		{"id": "6", "nodeId": "nUntagged", "name": "black-phone.tail.ts.net", "hostname": "black-phone",
			"lastSeen": "2026-10-01T00:00:00Z", "created": "2026-09-01T00:00:00Z"},
		{"id": "7", "nodeId": "nOnline", "name": "black-live.tail.ts.net", "hostname": "black-live", "tags": []string{"tag:devvm"},
			"lastSeen": "2026-10-01T00:00:00Z", "created": "2026-09-01T00:00:00Z", "connectedToControl": true},
	}
}

func TestDevices(t *testing.T) {
	f := &fakeAPI{devices: devicesFixture()}
	c := newTestClient(t, f)
	ctx := context.Background()

	d, err := c.FindDevice(ctx, "black-shop")
	if err != nil || d == nil || d.APIID() != "nShop" || !d.HasTag("tag:devvm") {
		t.Fatalf("FindDevice = %+v, %v", d, err)
	}
	if d.LastSeen.IsZero() || d.Created.IsZero() {
		t.Fatal("times not parsed")
	}
	// Matches by first label of name too.
	if d, _ := c.FindDevice(ctx, "black-old"); d == nil || d.NodeID != "nOld" {
		t.Fatalf("by name: %+v", d)
	}
	if d, _ := c.FindDevice(ctx, "nope"); d != nil {
		t.Fatalf("unexpected %+v", d)
	}

	n, err := c.DeleteByHostname(ctx, "black-shop")
	if err != nil || n != 1 || len(f.deleted) != 1 || f.deleted[0] != "nShop" {
		t.Fatalf("delete: n=%d err=%v deleted=%v", n, err, f.deleted)
	}
	if n, err := c.DeleteByHostname(ctx, "absent"); n != 0 || err != nil {
		t.Fatalf("absent: %d %v", n, err)
	}
	if err := c.DeleteDevice(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestOrphans(t *testing.T) {
	f := &fakeAPI{devices: devicesFixture()}
	c := newTestClient(t, f)
	devs, err := c.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 4, 0, 0, 0, time.UTC)
	got := Orphans(devs, OrphanFilter{
		Tag: "tag:devvm", Machine: "black", MinAge: 14 * 24 * time.Hour, Now: now,
		Known: map[string]bool{"black-shop": true},
	})
	var ids []string
	for _, d := range got {
		ids = append(ids, d.NodeID)
	}
	if strings.Join(ids, ",") != "nOld" {
		t.Fatalf("orphans = %v", ids)
	}
	// With no age threshold, the fresh one and the unknown shop qualify too.
	got = Orphans(devs, OrphanFilter{Tag: "tag:devvm", Machine: "black", Now: now})
	ids = nil
	for _, d := range got {
		ids = append(ids, d.NodeID)
	}
	if strings.Join(ids, ",") != "nShop,nOld,nFresh" {
		t.Fatalf("orphans = %v", ids)
	}
}

const statusJSON = `{
  "Version": "1.104.5", "BackendState": "Running", "HaveNodeKey": true,
  "MagicDNSSuffix": "taildbf6fc.ts.net",
  "CurrentTailnet": {"Name": "user.github", "MagicDNSSuffix": "taildbf6fc.ts.net", "MagicDNSEnabled": true},
  "CertDomains": null,
  "Self": {"HostName": "black", "DNSName": "black.taildbf6fc.ts.net."}
}`

func TestLocalStatus(t *testing.T) {
	f := (&run.Fake{}).On("tailscale status --json", statusJSON)
	s, err := LocalStatus(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if !s.LoggedIn() || !s.Running() || s.HTTPSEnabled() || !s.MagicDNSEnabled {
		t.Fatalf("%+v", s)
	}
	if s.MagicDNSSuffix != "taildbf6fc.ts.net" || s.TailnetName != "user.github" || s.SelfDNSName != "black.taildbf6fc.ts.net" {
		t.Fatalf("%+v", s)
	}
	s2, _ := ParseStatus([]byte(`{"BackendState":"Running","HaveNodeKey":true,"CertDomains":["x.ts.net"]}`))
	if !s2.HTTPSEnabled() {
		t.Fatal("https")
	}
	s3, _ := ParseStatus([]byte(`{"BackendState":"NeedsLogin"}`))
	if s3.LoggedIn() {
		t.Fatal("logged in")
	}
	// Non-zero exit with JSON still parses.
	f2 := &run.Fake{Rules: []run.Rule{{Prefix: "tailscale status", Func: func(c run.Cmd) (run.Result, error) {
		r := run.Result{Stdout: `{"BackendState":"NeedsLogin"}`, ExitCode: 1}
		return r, &run.ExitError{Cmd: c, Result: r}
	}}}}
	if s, err := LocalStatus(context.Background(), f2); err != nil || s.BackendState != "NeedsLogin" {
		t.Fatalf("%+v %v", s, err)
	}
	f3 := (&run.Fake{}).Fail("tailscale", 127, "not found")
	if _, err := LocalStatus(context.Background(), f3); err == nil {
		t.Fatal("want error")
	}
}

func TestURLs(t *testing.T) {
	cases := map[string]string{
		URL("black-shop", 3000):                     "http://black-shop:3000",
		URL("black-shop", 0):                        "http://black-shop",
		HTTPSURL("black-shop", "tail.ts.net", 3000): "https://black-shop.tail.ts.net:3000",
		HTTPSURL("black-shop", "tail.ts.net.", 443): "https://black-shop.tail.ts.net",
		strings.Join(ServeOnArgs(3000), " "):        "sudo tailscale serve --bg --yes --https=3000 http://127.0.0.1:3000",
		strings.Join(ServeOffArgs(3000), " "):       "sudo tailscale serve --yes --https=3000 off",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	if !IsServeNotFound("error: failed to remove web serve: handler does not exist") {
		t.Error("IsServeNotFound")
	}
}

func TestSetupGuide(t *testing.T) {
	g := SetupGuide("black")
	for _, s := range []string{
		`"tag:devvm": ["autogroup:admin"]`,
		`"src": ["autogroup:member"], "dst": ["tag:devvm"]`,
		"black-<name>", "MagicDNS: on", "HTTPS Certificates: leave OFF",
		"Auth Keys (write)", "Devices Core (read and write)", "REMOVE the default allow-all",
		"add-generic-password -U -s dev-tailscale -a client-secret -w",
	} {
		if !strings.Contains(g, s) {
			t.Errorf("guide lacks %q", s)
		}
	}
	// The snippet is valid JSON once comments are stripped.
	var lines []string
	for _, l := range strings.Split(PolicySnippet("tag:devvm"), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines = append(lines, l)
		}
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(strings.Join(lines, "\n")), &v); err != nil {
		t.Fatalf("policy snippet: %v", err)
	}
}
