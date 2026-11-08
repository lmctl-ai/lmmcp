package bridge

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
)

// Address policy. Always refused, for every remote server: link-local (169.254.0.0/16, fe80::/10, which includes the
// EC2/GCP/Azure metadata address 169.254.169.254), the IPv6 metadata address fd00:ec2::254, and unspecified addresses.
// Optionally refused (allowPrivateAddresses=false): RFC 1918, carrier-grade NAT, unique-local fc00::/7 and loopback,
// except the documented literal-loopback http form.
var errAddressRefused = errors.New("address refused")

var metadataV6 = net.ParseIP("fd00:ec2::254")

func normalizeIP(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

func parseLegacyIPv4(s string) (net.IP, bool) {
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 4 {
		return nil, false
	}
	var vals [4]uint64
	for i, p := range parts {
		if p == "" || p[0] < '0' || p[0] > '9' {
			return nil, false
		}
		v, err := strconv.ParseUint(p, 0, 32)
		if err != nil {
			return nil, false
		}
		vals[i] = v
	}
	for i := 0; i < len(parts)-1; i++ {
		if vals[i] > 0xff {
			return nil, false
		}
	}
	maxLast := (uint64(1) << (8 * uint(5-len(parts)))) - 1
	if vals[len(parts)-1] > maxLast {
		return nil, false
	}
	var packed uint32
	for i := 0; i < len(parts)-1; i++ {
		packed |= uint32(vals[i]) << uint(8*(3-i))
	}
	packed |= uint32(vals[len(parts)-1])
	return net.IPv4(byte(packed>>24), byte(packed>>16), byte(packed>>8), byte(packed)), true
}

func parseHostIP(host string) (net.IP, bool) {
	trimmed := strings.TrimRight(host, ".")
	if trimmed == "" {
		return nil, false
	}
	if addr, err := netip.ParseAddr(trimmed); err == nil {
		addr = addr.WithZone("").Unmap()
		s := addr.AsSlice()
		return net.IP(s), true
	}
	if ip := net.ParseIP(trimmed); ip != nil {
		return normalizeIP(ip), true
	}
	return parseLegacyIPv4(trimmed)
}

// ParseHostIP parses standard, zone-qualified IPv6, trailing-dot, and inet_aton IPv4 host literals.
func ParseHostIP(host string) net.IP {
	ip, ok := parseHostIP(host)
	if !ok {
		return nil
	}
	return ip
}

// forbiddenAddress reports addresses no remote MCP server may live at.
func forbiddenAddress(ip net.IP) bool {
	ip = normalizeIP(ip)
	return ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.Equal(metadataV6)
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// privateAddress reports addresses an operator may choose to refuse.
func privateAddress(ip net.IP) bool {
	ip = normalizeIP(ip)
	return ip.IsPrivate() || ip.IsLoopback() || cgnat.Contains(ip)
}

// dialControl runs for every connection attempt after name resolution, so a DNS name that resolves (or rebinds) to a
// forbidden address is refused at the socket, never connected.
func dialControl(allowPrivate, loopbackLiteral bool, refused func()) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, ok := parseHostIP(host)
		if !ok {
			if refused != nil {
				refused()
			}
			return errAddressRefused
		}
		if forbiddenAddress(ip) || !allowPrivate && privateAddress(ip) && !(loopbackLiteral && normalizeIP(ip).IsLoopback()) {
			if refused != nil {
				refused()
			}
			return errAddressRefused
		}
		return nil
	}
}

// SafeDialer is used by clients of a lmmcp REST endpoint: link-local and metadata destinations are never dialled.
func SafeDialer() *net.Dialer {
	return &net.Dialer{Control: dialControl(true, true, nil)}
}

// ForbiddenAddress is the literal-IP check shared with the CLI client.
func ForbiddenAddress(ip net.IP) bool { return forbiddenAddress(ip) }
