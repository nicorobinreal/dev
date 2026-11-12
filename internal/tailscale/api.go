package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the Tailscale control API.
const DefaultBaseURL = "https://api.tailscale.com"

// DefaultKeyExpiry is how long a minted auth key stays valid. It is used once,
// right after minting, so it can be short.
const DefaultKeyExpiry = 10 * time.Minute

// ErrNotFound is returned by DeleteDevice when the device does not exist.
var ErrNotFound = errors.New("not found")

// APIError is a non-2xx answer from the Tailscale API.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("tailscale API %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// CredentialSource yields the OAuth client lazily, so commands only touch the
// Keychain when they actually call the API. Keychain.Credentials fits.
type CredentialSource func(ctx context.Context) (Credentials, error)

// StaticCredentials wraps fixed credentials (tests).
func StaticCredentials(c Credentials) CredentialSource {
	return func(context.Context) (Credentials, error) { return c, nil }
}

// Client talks to the Tailscale API with an OAuth access token minted from
// the client-credentials flow. The token is cached in memory for the life of
// the Client (share one Client per process). Safe for concurrent use.
type Client struct {
	HTTP    *http.Client // nil: a client with a 30s timeout
	BaseURL string       // "": DefaultBaseURL
	Creds   CredentialSource
	Now     func() time.Time // nil: time.Now

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewClient returns a Client reading credentials from src.
func NewClient(src CredentialSource) *Client { return &Client{Creds: src} }

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

// Token returns a cached or freshly minted API access token.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expires) {
		return c.token, nil
	}
	if c.Creds == nil {
		return "", ErrNoCredentials
	}
	cr, err := c.Creds(ctx)
	if err != nil {
		return "", err
	}
	form := url.Values{"client_id": {cr.ClientID}, "client_secret": {cr.ClientSecret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/api/v2/oauth/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tr tokenResponse
	if err := c.do(req, &tr); err != nil {
		return "", fmt.Errorf("mint Tailscale API token: %w", err)
	}
	if tr.AccessToken == "" {
		return "", errors.New("mint Tailscale API token: empty access_token")
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	c.token = tr.AccessToken
	c.expires = c.now().Add(ttl - time.Minute) // refresh a little early
	return c.token, nil
}

// CheckToken mints an API token (for doctor).
func (c *Client) CheckToken(ctx context.Context) error {
	_, err := c.Token(ctx)
	return err
}

// do sends req and decodes a JSON body into out (if non-nil). Error bodies
// never include the request, so secrets don't leak into messages.
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(body))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return &APIError{Method: req.Method, Path: req.URL.Path, Status: resp.StatusCode, Message: msg}
	}
	if out == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

// api runs an authenticated request against /api/v2<path>.
func (c *Client) api(ctx context.Context, method, path string, in, out any) error {
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+"/api/v2"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, out)
}

// AuthKeyOptions configures MintAuthKey.
type AuthKeyOptions struct {
	Tag      string        // e.g. "tag:devvm" (required)
	Hostname string        // used for the description "dev <hostname>"
	Expiry   time.Duration // 0: DefaultKeyExpiry
}

type keyCreate struct {
	Reusable      bool     `json:"reusable"`
	Ephemeral     bool     `json:"ephemeral"`
	Preauthorized bool     `json:"preauthorized"`
	Tags          []string `json:"tags"`
}

type keyRequest struct {
	Capabilities struct {
		Devices struct {
			Create keyCreate `json:"create"`
		} `json:"devices"`
	} `json:"capabilities"`
	ExpirySeconds int    `json:"expirySeconds"`
	Description   string `json:"description,omitempty"`
}

// MintAuthKey creates a single-use, non-ephemeral, pre-authorized auth key
// tagged opts.Tag and returns the key string ("tskey-auth-...").
func (c *Client) MintAuthKey(ctx context.Context, opts AuthKeyOptions) (string, error) {
	if opts.Tag == "" {
		return "", errors.New("mint auth key: tag required")
	}
	exp := opts.Expiry
	if exp <= 0 {
		exp = DefaultKeyExpiry
	}
	var req keyRequest
	req.Capabilities.Devices.Create = keyCreate{Preauthorized: true, Tags: []string{opts.Tag}}
	req.ExpirySeconds = int(exp / time.Second)
	req.Description = keyDescription(opts.Hostname)
	var resp struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := c.api(ctx, http.MethodPost, "/tailnet/-/keys", req, &resp); err != nil {
		return "", fmt.Errorf("mint auth key: %w", err)
	}
	if resp.Key == "" {
		return "", errors.New("mint auth key: response has no key")
	}
	return resp.Key, nil
}

// keyDescription builds "dev <hostname>", limited to the API's 50 characters
// and to characters it accepts (letters, digits, '-', '_', ' ').
func keyDescription(host string) string {
	if host == "" {
		return "dev"
	}
	var b strings.Builder
	for _, r := range "dev " + host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := b.String()
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}

// Device is a tailnet node as returned by GET /tailnet/-/devices.
type Device struct {
	ID                 string    `json:"id"`     // legacy numeric ID
	NodeID             string    `json:"nodeId"` // preferred stable ID
	Name               string    `json:"name"`   // MagicDNS FQDN, e.g. black-shop.tailxxxx.ts.net
	Hostname           string    `json:"hostname"`
	Tags               []string  `json:"tags"`
	Addresses          []string  `json:"addresses"`
	OS                 string    `json:"os"`
	Created            time.Time `json:"created"`
	LastSeen           time.Time `json:"lastSeen"`
	ConnectedToControl bool      `json:"connectedToControl"`
}

// ShortName is the first label of Name (the MagicDNS host name, which may
// differ from Hostname when Tailscale de-duplicated it, e.g. "x-1").
func (d Device) ShortName() string {
	n, _, _ := strings.Cut(d.Name, ".")
	return n
}

// HasTag reports whether the device carries tag.
func (d Device) HasTag(tag string) bool {
	for _, t := range d.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Matches reports whether the device is the node called host (compares both
// Hostname and the first label of Name, case-insensitively).
func (d Device) Matches(host string) bool {
	return strings.EqualFold(d.Hostname, host) || strings.EqualFold(d.ShortName(), host)
}

// APIID is the ID to use in /device/{id} calls.
func (d Device) APIID() string {
	if d.NodeID != "" {
		return d.NodeID
	}
	return d.ID
}

// ListDevices returns every device in the tailnet.
func (c *Client) ListDevices(ctx context.Context) ([]Device, error) {
	var resp struct {
		Devices []Device `json:"devices"`
	}
	if err := c.api(ctx, http.MethodGet, "/tailnet/-/devices", nil, &resp); err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	return resp.Devices, nil
}

// FindDevices returns every device matching host (normally zero or one; a
// renamed duplicate like "host-1" is not a match, but the original is).
func (c *Client) FindDevices(ctx context.Context, host string) ([]Device, error) {
	devs, err := c.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	var out []Device
	for _, d := range devs {
		if d.Matches(host) {
			out = append(out, d)
		}
	}
	return out, nil
}

// FindDevice returns the first device matching host, or nil if none.
func (c *Client) FindDevice(ctx context.Context, host string) (*Device, error) {
	ds, err := c.FindDevices(ctx, host)
	if err != nil || len(ds) == 0 {
		return nil, err
	}
	return &ds[0], nil
}

// DeleteDevice removes a node by API ID. A missing device yields ErrNotFound.
func (c *Client) DeleteDevice(ctx context.Context, id string) error {
	err := c.api(ctx, http.MethodDelete, "/device/"+url.PathEscape(id), nil, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return fmt.Errorf("delete device %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("delete device %s: %w", id, err)
	}
	return nil
}

// DeleteByHostname deletes every node matching host and returns how many it
// deleted (0 when the node is already gone, which is not an error).
func (c *Client) DeleteByHostname(ctx context.Context, host string) (int, error) {
	ds, err := c.FindDevices(ctx, host)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range ds {
		if err := c.DeleteDevice(ctx, d.APIID()); err != nil && !errors.Is(err, ErrNotFound) {
			return n, err
		}
		n++
	}
	return n, nil
}

// OrphanFilter selects tailnet nodes dev created but no longer knows about.
type OrphanFilter struct {
	Tag     string          // e.g. "tag:devvm"
	Machine string          // only nodes named "<Machine>-..." (this Mac's)
	Known   map[string]bool // Tailscale hostnames of existing projects
	MinAge  time.Duration   // not seen for at least this long
	Now     time.Time       // zero: time.Now()
}

// Orphans returns the devices that carry f.Tag, are named "<machine>-*", are
// not in f.Known, are not connected, and were last seen (or, if never seen,
// created) at least f.MinAge ago.
func Orphans(devs []Device, f OrphanFilter) []Device {
	now := f.Now
	if now.IsZero() {
		now = time.Now()
	}
	prefix := strings.ToLower(f.Machine + "-")
	var out []Device
	for _, d := range devs {
		if !d.HasTag(f.Tag) || d.ConnectedToControl {
			continue
		}
		host := strings.ToLower(d.Hostname)
		short := strings.ToLower(d.ShortName())
		if !strings.HasPrefix(host, prefix) && !strings.HasPrefix(short, prefix) {
			continue
		}
		if f.Known[host] || f.Known[short] || f.Known[d.Hostname] || f.Known[d.ShortName()] {
			continue
		}
		seen := d.LastSeen
		if seen.IsZero() {
			seen = d.Created
		}
		if now.Sub(seen) < f.MinAge {
			continue
		}
		out = append(out, d)
	}
	return out
}
