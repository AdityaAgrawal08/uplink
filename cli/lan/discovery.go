package lan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

type ServiceInfo struct {
	Hostname         string
	Port             int
	ShareCode        string
	FileName         string
	Size             int64
	Fingerprint      string
	FileSHA256       string
	PasswordRequired bool
}

// NOTE: InfoFields broadcast filename/size/sha/fingerprint in plaintext to
// the LAN segment (mDNS TXT). Only the share code is hashed (8 hex chars /
// 32 bits — brute-forceable locally). The TLS fingerprint pin protects the
// transfer itself, not the metadata. For sensitive names, callers should
// pass an opaque session token as Hostname/FileName or document the exposure.
func RegisterService(info ServiceInfo) (func(), error) {
	// 1. Hash shareCode to prevent exposure in local broadcast networks
	sum := sha256.Sum256([]byte(info.ShareCode))
	hashedShareCode := hex.EncodeToString(sum[:])[:8]

	passwordRequiredStr := "false"
	if info.PasswordRequired {
		passwordRequiredStr = "true"
	}

	service, err := mdns.NewMDNSService(
		info.Hostname,
		"_uplink._tcp",
		"", // domain - empty = local
		"", // host - empty = auto-detect
		info.Port,
		nil, // IPs - empty = auto-detect
		[]string{
			hashedShareCode,
			info.FileName,
			strconv.FormatInt(info.Size, 10),
			info.Fingerprint,
			info.FileSHA256,
			passwordRequiredStr,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("mDNS service create: %w", err)
	}
	server, err := mdns.NewServer(&mdns.Config{Zone: service})
	if err != nil {
		return nil, fmt.Errorf("mDNS server start: %w", err)
	}
	return func() { _ = server.Shutdown() }, nil
}

// DiscoverService matches peer on local subnet, resolving details
// Returns addr, filename, size, fingerprint, fileSHA256, passwordRequired, err
func DiscoverService(ctx context.Context, shareCode string) (string, string, int64, string, string, bool, error) {
	entriesCh := make(chan *mdns.ServiceEntry, 20)

	qp := &mdns.QueryParam{
		Service: "_uplink._tcp",
		Domain:  "local",
		Timeout: 3 * time.Second,
		Entries: entriesCh,
	}

	// Compute expected hashed code to match advertised candidates
	sum := sha256.Sum256([]byte(shareCode))
	expectedHashedCode := hex.EncodeToString(sum[:])[:8]

	go func() {
		defer close(entriesCh)
		_ = mdns.Query(qp)
	}()

	for {
		select {
		case entry, ok := <-entriesCh:
			if !ok {
				return "", "", 0, "", "", false, fmt.Errorf("peer not found on LAN (mDNS search completed)")
			}
			if len(entry.InfoFields) >= 6 && entry.InfoFields[0] == expectedHashedCode {
				// Test AddrV4 for nil BEFORE String(): nil.String() yields "<nil>"
				// which would become the literal host. Prefer IPv4, then IPv6.
				var host string
				if entry.AddrV4 != nil {
					host = entry.AddrV4.String()
				} else if entry.AddrV6 != nil {
					host = entry.AddrV6.String()
				} else {
					host = strings.TrimSuffix(entry.Host, ".")
				}
				host = strings.TrimSuffix(host, ".")
				addr := net.JoinHostPort(host, strconv.Itoa(entry.Port))
				filename := entry.InfoFields[1]
				size, _ := strconv.ParseInt(entry.InfoFields[2], 10, 64)
				fingerprint := entry.InfoFields[3]
				fileSHA256 := entry.InfoFields[4]
				passwordRequired := entry.InfoFields[5] == "true"
				return addr, filename, size, fingerprint, fileSHA256, passwordRequired, nil
			}
		case <-ctx.Done():
			return "", "", 0, "", "", false, ctx.Err()
		}
	}
}
