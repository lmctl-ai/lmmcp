package bridge

import (
	"errors"
	"net"
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
		ip := net.ParseIP(host)
		if ip == nil {
			return nil
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
