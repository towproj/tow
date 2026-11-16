package cloud

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// checkIP answers with the caller's public address; it asks for no account.
var checkIP = "https://checkip.amazonaws.com"

// PublicCIDR asks which address this machine reaches the internet from: machines on a public
// cloud have public addresses, so that is the address they see. SSH and the API are opened to
// it alone.
func PublicCIDR(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkIP, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("find this machine's public IP (SSH is opened to it only): %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("unexpected answer from %s: %q", checkIP, strings.TrimSpace(string(body)))
	}
	return ip.String() + "/32", nil
}
