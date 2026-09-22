package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Eastsidegunn/JANUS/seams/world/local/egressproxy"
)

type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hxegressproxy:", err)
		os.Exit(1)
	}
}

func run() error {
	var allowlist stringsFlag
	var inject stringsFlag
	listen := flag.String("listen", ":3128", "HTTP proxy listen address")
	auditSocket := flag.String("audit-socket", "/run/hx-audit/audit.sock", "host audit socket")
	credentialSocket := flag.String("credential-socket", "", "host credential socket (required if --inject is set)")
	flag.Var(&allowlist, "allow", "allowed DNS domain (repeatable)")
	flag.Var(&inject, "inject", "credential injection rule domain|header|valueprefix|credname (repeatable)")
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flag.Args())
	}
	rules, names, err := parseInjectRules(inject)
	if err != nil {
		return err
	}
	// Fetch credential VALUES over the host socket into memory before binding.
	// The value is never on disk, argv, or env — only the (non-secret) names
	// arrive via --inject. Fail closed if a rule's credential cannot be fetched.
	credentials := map[string]string{}
	if len(names) > 0 {
		if *credentialSocket == "" {
			return fmt.Errorf("--inject requires --credential-socket")
		}
		fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		credentials, err = egressproxy.UnixCredentialSource{Path: *credentialSocket}.Fetch(fetchCtx, names)
		cancel()
		if err != nil {
			return fmt.Errorf("fetch credentials: %w", err)
		}
	}
	sink := egressproxy.UnixAuditSink{Path: *auditSocket}
	proxy, err := egressproxy.New(egressproxy.Config{
		Allowlist: allowlist, Audit: sink, Inject: rules, Credentials: credentials,
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

// parseInjectRules decodes repeatable domain|header|valueprefix|credname rules.
// Only names are decoded here; the credential value is fetched separately.
func parseInjectRules(raw []string) ([]egressproxy.InjectRule, []string, error) {
	rules := make([]egressproxy.InjectRule, 0, len(raw))
	seen := map[string]bool{}
	names := make([]string, 0, len(raw))
	for _, entry := range raw {
		parts := strings.SplitN(entry, "|", 4)
		if len(parts) != 4 {
			return nil, nil, fmt.Errorf("inject rule은 domain|header|valueprefix|credname 형식이어야 함: %q", entry)
		}
		rule := egressproxy.InjectRule{Domain: parts[0], Header: parts[1], ValuePrefix: parts[2], CredentialName: parts[3]}
		if rule.Domain == "" || rule.Header == "" || rule.CredentialName == "" {
			return nil, nil, fmt.Errorf("inject rule의 domain/header/credname은 비어 있을 수 없음: %q", entry)
		}
		rules = append(rules, rule)
		if !seen[rule.CredentialName] {
			seen[rule.CredentialName] = true
			names = append(names, rule.CredentialName)
		}
	}
	return rules, names, nil
}
