package main

import (
	"net/netip"
	"testing"
)

// SCP-T26-001 §3.2: --pin 반복 플래그 파싱은 world-config와 같은 규칙.
func TestParsePinsFlags(t *testing.T) {
	pins, err := parsePins([]string{"gw.example=100.64.0.10:8080", "Other.Example=10.0.0.5:443"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]netip.AddrPort{
		"gw.example":    netip.MustParseAddrPort("100.64.0.10:8080"),
		"other.example": netip.MustParseAddrPort("10.0.0.5:443"),
	}
	if len(pins) != len(want) {
		t.Fatalf("pins = %v", pins)
	}
	for domain, address := range want {
		if pins[domain] != address {
			t.Fatalf("pins[%q] = %v, want %v", domain, pins[domain], address)
		}
	}
	if pins, err := parsePins(nil); err != nil || len(pins) != 0 {
		t.Fatalf("pin 없음 = %v, %v", pins, err)
	}
	for name, values := range map[string][]string{
		"중복":   {"gw.example=100.64.0.10:8080", "GW.example=10.0.0.5:443"},
		"형식":   {"gw.example:100.64.0.10:8080"},
		"호스트명": {"gw.example=gateway.lan:8080"},
		"상한 초과": {
			"a.example=10.0.0.1:1", "b.example=10.0.0.1:1", "c.example=10.0.0.1:1",
			"d.example=10.0.0.1:1", "e.example=10.0.0.1:1",
		},
	} {
		if _, err := parsePins(values); err == nil {
			t.Errorf("%s: 불량 --pin %v 수용", name, values)
		}
	}
}
