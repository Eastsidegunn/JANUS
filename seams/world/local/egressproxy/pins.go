package egressproxy

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// MaxPins bounds operator-declared gateway pins per span (SCP-T26-001 §3.1).
const MaxPins = 4

// Pin is one operator-declared gateway pin: an allowlisted DNS domain whose
// address is fixed by the operator instead of resolved through DNS. Pins live
// only in the operator-owned world config, never in policy profiles/overlays,
// so they cannot widen the egress allowlist (FR-SBX-03, SCP-T26-001 §4).
type Pin struct {
	Domain  string
	Address netip.AddrPort
}

// ParsePinAddress accepts only an exact ip:port (no hostname, no zone, no
// IPv4-mapped IPv6) and applies the pin address rules. Private and RFC 6598
// (CGNAT) addresses are deliberately allowed — that is the pin's purpose.
func ParsePinAddress(address string) (netip.AddrPort, error) {
	parsed, err := netip.ParseAddrPort(address)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("pin address %q는 정확한 ip:port여야 함", address)
	}
	if err := validatePinAddress(parsed); err != nil {
		return netip.AddrPort{}, fmt.Errorf("pin address %q: %w", address, err)
	}
	return parsed, nil
}

// ParsePinFlag parses the sidecar's repeatable `--pin <domain>=<ip:port>`.
func ParsePinFlag(value string) (Pin, error) {
	domain, address, ok := strings.Cut(value, "=")
	if !ok {
		return Pin{}, fmt.Errorf("pin %q는 domain=ip:port 형식이어야 함", value)
	}
	parsed, err := ParsePinAddress(address)
	if err != nil {
		return Pin{}, err
	}
	normalized, err := normalizePinDomain(domain)
	if err != nil {
		return Pin{}, err
	}
	return Pin{Domain: normalized, Address: parsed}, nil
}

// NormalizePins validates a whole pin set fail-closed: every domain follows
// the allowlist normalization rule (lowercase ASCII DNS name, no IP literal),
// domains are unique after normalization, every address follows the pin
// address rules, and the set holds at most MaxPins entries. The host world
// config parser and the sidecar both run this, so they cannot disagree.
func NormalizePins(pins []Pin) (map[string]netip.AddrPort, error) {
	if len(pins) > MaxPins {
		return nil, fmt.Errorf("pin 개수 %d가 상한 %d 초과", len(pins), MaxPins)
	}
	normalized := make(map[string]netip.AddrPort, len(pins))
	for _, pin := range pins {
		domain, err := normalizePinDomain(pin.Domain)
		if err != nil {
			return nil, err
		}
		if _, duplicate := normalized[domain]; duplicate {
			return nil, fmt.Errorf("pin domain %q 중복", domain)
		}
		if err := validatePinAddress(pin.Address); err != nil {
			return nil, fmt.Errorf("pin %q address %s: %w", domain, pin.Address, err)
		}
		normalized[domain] = pin.Address
	}
	return normalized, nil
}

// PinDomains returns the pinned domains in a stable order.
func PinDomains(pins map[string]netip.AddrPort) []string {
	domains := make([]string, 0, len(pins))
	for domain := range pins {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	return domains
}

// AllowlistPermits applies the proxy's allowlist matching rule (exact domain
// or a label-boundary subdomain) to an already normalized allowlist. Prepare
// uses it so a pin outside the merged policy fails before any runtime effect.
func AllowlistPermits(allowlist []string, domain string) bool {
	for _, allowed := range allowlist {
		if domain == allowed || strings.HasSuffix(domain, "."+allowed) {
			return true
		}
	}
	return false
}

func normalizePinDomain(domain string) (string, error) {
	normalized, err := normalizeDomain(domain)
	if err != nil {
		return "", fmt.Errorf("pin domain %q: %w", domain, err)
	}
	return normalized, nil
}

func validatePinAddress(address netip.AddrPort) error {
	ip := address.Addr()
	switch {
	case !address.IsValid() || !ip.IsValid():
		return errors.New("유효한 ip:port가 아님")
	case ip.Zone() != "":
		return errors.New("IPv6 zone은 허용하지 않음")
	case ip.Is4In6():
		return errors.New("IPv4-mapped IPv6는 허용하지 않음(IPv4로 표기)")
	case ip.IsLoopback():
		return errors.New("loopback 주소는 pin할 수 없음")
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return errors.New("link-local 주소는 pin할 수 없음")
	case ip.IsMulticast():
		return errors.New("multicast 주소는 pin할 수 없음")
	case ip.IsUnspecified():
		return errors.New("unspecified 주소는 pin할 수 없음")
	case address.Port() == 0:
		return errors.New("port는 1~65535여야 함")
	}
	return nil
}
