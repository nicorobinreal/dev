// Package tailscale holds everything dev does with Tailscale: Keychain
// access for the OAuth client, the API client (token, auth keys, devices),
// local CLI helpers, the one-time setup guide, and `tailscale serve` helpers.
package tailscale

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nicorobinreal/dev/internal/run"
)

// Keychain item coordinates.
const (
	KeychainService     = "dev-tailscale"
	AccountClientID     = "client-id"
	AccountClientSecret = "client-secret"
	SecurityBin         = "/usr/bin/security"
)

// ErrNoCredentials means the Keychain has no item for the account.
var ErrNoCredentials = errors.New("tailscale OAuth client not in Keychain (run `dev doctor`)")

// securityItemNotFound is the exit status `security` uses for
// errSecItemNotFound.
const securityItemNotFound = 44

// Credentials is the OAuth client used for the client-credentials flow.
type Credentials struct {
	ClientID     string
	ClientSecret string
}

// Keychain reads and writes the dev-tailscale items by shelling out to
// /usr/bin/security (never the Security framework: items created by
// `security` trust that tool, so a self-updated dev binary never triggers a
// Keychain GUI prompt).
type Keychain struct {
	R run.Runner
	// Bin overrides /usr/bin/security (tests).
	Bin string
}

func (k Keychain) bin() string {
	if k.Bin != "" {
		return k.Bin
	}
	return SecurityBin
}

// Get returns the password stored for account. A missing item yields an
// error wrapping ErrNoCredentials.
func (k Keychain) Get(ctx context.Context, account string) (string, error) {
	res, err := k.R.Run(ctx, run.Cmd{Name: k.bin(), Args: []string{
		"find-generic-password", "-s", KeychainService, "-a", account, "-w",
	}})
	if err != nil {
		var ee *run.ExitError
		if errors.As(err, &ee) && (ee.Result.ExitCode == securityItemNotFound ||
			strings.Contains(ee.Result.Stderr, "could not be found")) {
			return "", fmt.Errorf("%w: account %s", ErrNoCredentials, account)
		}
		return "", fmt.Errorf("read Keychain %s/%s: %w", KeychainService, account, err)
	}
	v := strings.TrimRight(res.Stdout, "\r\n")
	if v == "" {
		return "", fmt.Errorf("%w: account %s is empty", ErrNoCredentials, account)
	}
	return v, nil
}

// Credentials reads both the client ID and the secret.
func (k Keychain) Credentials(ctx context.Context) (Credentials, error) {
	id, err := k.Get(ctx, AccountClientID)
	if err != nil {
		return Credentials{}, err
	}
	sec, err := k.Get(ctx, AccountClientSecret)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{ClientID: id, ClientSecret: sec}, nil
}

// Has reports whether both items exist (for doctor).
func (k Keychain) Has(ctx context.Context) (bool, error) {
	_, err := k.Credentials(ctx)
	if errors.Is(err, ErrNoCredentials) {
		return false, nil
	}
	return err == nil, err
}

// Set stores value for account (-U updates an existing item). The value is
// passed as `-w <value>` on argv, so it is briefly visible in `ps`: use it
// for the client ID, and for the secret only when no person is at a
// terminal. `security` has no way to read the password from a pipe when a
// controlling terminal exists (it opens /dev/tty), see SetInteractive.
func (k Keychain) Set(ctx context.Context, account, value string) error {
	if value == "" {
		return fmt.Errorf("refusing to store an empty %s", account)
	}
	_, err := k.R.Run(ctx, run.Cmd{Name: k.bin(), Args: []string{
		"add-generic-password", "-U", "-s", KeychainService, "-a", account, "-w", value,
	}})
	if err != nil {
		return fmt.Errorf("store Keychain %s/%s: %w", KeychainService, account, err)
	}
	return nil
}

// SetInteractive lets `security` itself prompt the person for the value
// (`-w` given last with no value: it asks twice on the terminal), so the
// secret never touches dev's argv or memory. For the guided doctor flow.
func (k Keychain) SetInteractive(ctx context.Context, account string) error {
	_, err := k.R.Run(ctx, run.Cmd{Name: k.bin(), Interactive: true, Args: []string{
		"add-generic-password", "-U", "-s", KeychainService, "-a", account, "-w",
	}})
	if err != nil {
		return fmt.Errorf("store Keychain %s/%s: %w", KeychainService, account, err)
	}
	return nil
}

// Delete removes the item for account; a missing item is not an error.
func (k Keychain) Delete(ctx context.Context, account string) error {
	_, err := k.R.Run(ctx, run.Cmd{Name: k.bin(), Args: []string{
		"delete-generic-password", "-s", KeychainService, "-a", account,
	}})
	var ee *run.ExitError
	if errors.As(err, &ee) && ee.Result.ExitCode == securityItemNotFound {
		return nil
	}
	return err
}
