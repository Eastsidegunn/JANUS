package local

import (
	"fmt"
	"net/netip"

	"github.com/Eastsidegunn/JANUS/seams/world/local/egressproxy"
)

// EgressPinConfig is the operator world-config form of a declared gateway pin
// (SCP-T26-001 §3.1): an exact DNS domain and an exact ip:port.
type EgressPinConfig struct {
	Domain  string
	Address string
}

// NormalizeEgressPins validates operator-declared pins with the same rules
// the sidecar re-applies (egressproxy.NormalizePins). The host calls it before
// any key claim so a malformed pin never consumes an idempotency key.
func NormalizeEgressPins(entries []EgressPinConfig) (map[string]netip.AddrPort, error) {
	if len(entries) > egressproxy.MaxPins {
		return nil, fmt.Errorf("egress pin 개수 %d가 상한 %d 초과", len(entries), egressproxy.MaxPins)
	}
	pins := make([]egressproxy.Pin, 0, len(entries))
	for _, entry := range entries {
		address, err := egressproxy.ParsePinAddress(entry.Address)
		if err != nil {
			return nil, err
		}
		pins = append(pins, egressproxy.Pin{Domain: entry.Domain, Address: address})
	}
	return egressproxy.NormalizePins(pins)
}

// ValidateEgressPinsWithinPolicy rejects a pin whose domain the merged policy
// egress allowlist does not permit. Pins only change how an allowed domain is
// addressed; they never add a domain (FR-SBX-03 narrowing-only).
func ValidateEgressPinsWithinPolicy(pins map[string]netip.AddrPort, egress []string) error {
	allowlist, err := egressproxy.NormalizeAllowlist(egress)
	if err != nil {
		return fmt.Errorf("egress policy: %w", err)
	}
	return egressPinsWithinAllowlist(pins, allowlist)
}

func egressPinsWithinAllowlist(pins map[string]netip.AddrPort, allowlist []string) error {
	for _, domain := range egressproxy.PinDomains(pins) {
		if !egressproxy.AllowlistPermits(allowlist, domain) {
			return fmt.Errorf("egress pin domain %q가 병합 정책 egress allowlist 밖", domain)
		}
	}
	return nil
}

func normalizePinMap(pins map[string]netip.AddrPort) (map[string]netip.AddrPort, error) {
	list := make([]egressproxy.Pin, 0, len(pins))
	for domain, address := range pins {
		list = append(list, egressproxy.Pin{Domain: domain, Address: address})
	}
	return egressproxy.NormalizePins(list)
}
