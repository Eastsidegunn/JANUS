package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"time"

	"github.com/Eastsidegunn/JANUS/core/buildinfo"
	"github.com/Eastsidegunn/JANUS/seams/world/local/egressproxy"
)

type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.Line("hxegressproxy"))
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hxegressproxy:", err)
		os.Exit(1)
	}
}

func run() error {
	var allowlist stringsFlag
	listen := flag.String("listen", ":3128", "HTTP proxy listen address")
	auditSocket := flag.String("audit-socket", "/run/hx-audit/audit.sock", "host audit socket")
	flag.Var(&allowlist, "allow", "allowed DNS domain (repeatable)")
	var pinFlags stringsFlag
	flag.Var(&pinFlags, "pin", "declared gateway pin domain=ip:port (repeatable)")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	pins, err := parsePins(pinFlags)
	if err != nil {
		return err
	}
	sink := egressproxy.UnixAuditSink{Path: *auditSocket}
	proxy, err := egressproxy.New(egressproxy.Config{
		Allowlist: allowlist, Pins: pins, Audit: sink,
	})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	// Bind before Ready: the host starts the agent only after this ACK, so an
	// allowed first request cannot race a sidecar that is not listening yet.
	if err := sink.Ready(context.Background()); err != nil {
		return fmt.Errorf("ready audit: %w", err)
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 15 * time.Second}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// parsePins turns repeated --pin flags into the validated pin set; duplicates
// are rejected here (not silently merged by the map) and New re-validates.
func parsePins(values []string) (map[string]netip.AddrPort, error) {
	pins := make([]egressproxy.Pin, 0, len(values))
	for _, value := range values {
		pin, err := egressproxy.ParsePinFlag(value)
		if err != nil {
			return nil, err
		}
		pins = append(pins, pin)
	}
	return egressproxy.NormalizePins(pins)
}
