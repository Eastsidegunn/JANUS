package policy

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

var exactVersionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
var sha256RE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// NormalizeExtension validates a concrete extension declaration and returns a
// canonical copy. It performs no registry or filesystem I/O; callers can use
// it before provisioning to guarantee that rejected declarations have no side
// effects.
func NormalizeExtension(ext gen.Extension) (gen.Extension, error) {
	if !exactVersionRE.MatchString(ext.Version) {
		return gen.Extension{}, fmt.Errorf("extension %q: version must be an exact semantic version", ext.Name)
	}
	if !sha256RE.MatchString(ext.Integrity) {
		return gen.Extension{}, fmt.Errorf("extension %q: integrity must be sha256 digest", ext.Name)
	}
	source, err := canonicalRegistry(ext.Source)
	if err != nil {
		return gen.Extension{}, fmt.Errorf("extension %q: invalid source", ext.Name)
	}
	if ext.Name == "" || strings.TrimSpace(ext.Name) != ext.Name {
		return gen.Extension{}, fmt.Errorf("extension name must be non-empty")
	}
	egress, err := canonicalEgress(ext.Egress)
	if err != nil {
		return gen.Extension{}, fmt.Errorf("extension %q: invalid egress", ext.Name)
	}
	ext.Source = source
	ext.Egress = egress
	return ext, nil
}

func normalizeAndAuthorizeExtensions(exts []gen.Extension, allowedNames, allowedRegistries []string) ([]gen.Extension, error) {
	if len(exts) == 0 {
		return nil, nil
	}
	names := map[string]bool{}
	for _, selector := range allowedNames {
		if canonical, err := canonicalExtensionSelector(selector); err == nil {
			names[canonical] = true
		}
	}
	registries := map[string]bool{}
	for _, registry := range allowedRegistries {
		if canonical := canonicalHostForPolicy(registry); canonical != "" {
			registries[canonical] = true
		}
	}
	out := make([]gen.Extension, 0, len(exts))
	seen := map[string]bool{}
	for _, raw := range exts {
		ext, err := NormalizeExtension(raw)
		if err != nil {
			return nil, err
		}
		identity := strings.Join([]string{ext.Name, ext.Version, ext.Integrity, ext.Source}, "\x00")
		if seen[identity] {
			return nil, fmt.Errorf("extension %q: duplicate declaration", ext.Name)
		}
		seen[identity] = true
		if !(names[ext.Name] || names[ext.Name+"@"+ext.Source]) {
			return nil, fmt.Errorf("extension %q: not allowed by policy", ext.Name)
		}
		if !registries[ext.Source] {
			return nil, fmt.Errorf("extension %q: source registry not allowed by policy", ext.Name)
		}
		out = append(out, ext)
	}
	sort.Slice(out, func(i, j int) bool {
		return extensionOrderKey(out[i]) < extensionOrderKey(out[j])
	})
	return out, nil
}

func extensionOrderKey(ext gen.Extension) string {
	return strings.Join([]string{ext.Name, ext.Version, ext.Source, ext.Integrity}, "\x00")
}

func cloneExtensions(in []gen.Extension) []gen.Extension {
	out := make([]gen.Extension, len(in))
	for i, ext := range in {
		out[i] = ext
		out[i].Egress = append([]string(nil), ext.Egress...)
	}
	return out
}

func canonicalRegistry(raw string) (string, error) {
	host := canonicalHostForPolicy(raw)
	if host == "" || net.ParseIP(host) != nil || strings.ContainsAny(strings.TrimSpace(raw), "/?#@ :") {
		return "", fmt.Errorf("registry host required")
	}
	return host, nil
}

func canonicalEgress(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		host := canonicalHostForPolicy(raw)
		if host == "" || net.ParseIP(host) != nil || strings.ContainsAny(strings.TrimSpace(raw), "/?#@ :") {
			return nil, fmt.Errorf("domain must be a hostname")
		}
		if !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	sort.Strings(out)
	return out, nil
}

func canonicalHostForPolicy(raw string) string {
	host := strings.ToLower(strings.TrimSpace(raw))
	host = strings.TrimSuffix(host, ".")
	return host
}

func canonicalProfileRegistries(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		host, err := canonicalRegistry(raw)
		if err != nil {
			return nil, fmt.Errorf("policy: allowed_registries %q — invalid registry host", raw)
		}
		if !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	sort.Strings(out)
	return out, nil
}

func intersectExtensionSelectors(a, b []string) []string {
	type selector struct{ name, source string }
	parse := func(raw string) (selector, bool) {
		canonical, err := canonicalExtensionSelector(raw)
		if err != nil {
			return selector{}, false
		}
		if i := strings.LastIndexByte(canonical, '@'); i >= 0 {
			return selector{name: canonical[:i], source: canonical[i+1:]}, true
		}
		return selector{name: canonical}, true
	}
	left := make([]selector, 0, len(a))
	for _, raw := range a {
		if s, ok := parse(raw); ok {
			left = append(left, s)
		}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range b {
		right, ok := parse(raw)
		if !ok {
			continue
		}
		for _, l := range left {
			if l.name != right.name || (l.source != "" && right.source != "" && l.source != right.source) {
				continue
			}
			source := l.source
			if source == "" {
				source = right.source
			}
			value := right.name
			if source != "" {
				value += "@" + source
			}
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
	}
	sort.Strings(out)
	return out
}

func canonicalExtensionSelector(raw string) (string, error) {
	selector := strings.TrimSpace(raw)
	if selector == "" || strings.TrimSpace(selector) != selector {
		return "", fmt.Errorf("extension selector is empty")
	}
	if i := strings.LastIndexByte(selector, '@'); i >= 0 {
		name := selector[:i]
		source, err := canonicalRegistry(selector[i+1:])
		if name == "" || err != nil {
			return "", fmt.Errorf("invalid extension selector")
		}
		return name + "@" + source, nil
	}
	return selector, nil
}
