package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
)

// dumpConfigCmd is read-only. It parses and merges profiles, then calls the
// same policy.Evaluate used by the run surface before writing one complete,
// deterministic JSON tree. No host-only world capability is accepted as input
// or serialized here.
func dumpConfigCmd(args []string) error {
	fs := flag.NewFlagSet("dump-config", flag.ContinueOnError)
	profilePath := fs.String("profile", "", "기본 정책 프로파일 YAML 경로 (필수)")
	overlays := stringListFlag{}
	fs.Var(&overlays, "overlay", "추가 정책 프로파일 YAML 경로 (반복 가능)")
	workspace := fs.String("workspace", "/workspace", "평가할 워크스페이스 절대 경로")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *profilePath == "" && fs.NArg() == 1 {
		*profilePath = fs.Arg(0)
	}
	if *profilePath == "" || fs.NArg() > 1 {
		return fmt.Errorf("사용법: hx dump-config --profile <file> [--overlay <file> ...] [--workspace <path>]")
	}
	base, err := readProfile(*profilePath)
	if err != nil {
		return err
	}
	profiles := make([]policy.Profile, 0, len(overlays))
	for _, path := range overlays {
		profile, err := readProfile(path)
		if err != nil {
			return err
		}
		profiles = append(profiles, profile)
	}
	rendered, err := renderDumpConfig(base, profiles, *workspace)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(rendered)
	return err
}

type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }
func (f *stringListFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("빈 overlay 경로")
	}
	*f = append(*f, value)
	return nil
}

func readProfile(path string) (policy.Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return policy.Profile{}, fmt.Errorf("profile %s 읽기: %w", path, err)
	}
	profile, err := policy.ParseProfile(data)
	if err != nil {
		return policy.Profile{}, fmt.Errorf("profile %s: %w", path, err)
	}
	return profile, nil
}

type dumpConfigTree struct {
	ProfileID         string              `json:"profile_id"`
	FSScope           []string            `json:"fs_scope"`
	Workspace         string              `json:"workspace"`
	Egress            []string            `json:"egress"`
	DeniedEgress      []string            `json:"denied_egress"`
	AllowedExtensions []string            `json:"allowed_extensions"`
	AllowedRegistries []string            `json:"allowed_registries"`
	Extensions        []dumpExtension     `json:"extensions"`
	Budget            gen.Budget          `json:"budget"`
	Approval          policy.ApprovalMode `json:"approval"`
	Redaction         string              `json:"redaction"`
}

type dumpExtension struct {
	Name           string   `json:"name"`
	Version        string   `json:"version"`
	Integrity      string   `json:"integrity"`
	Source         string   `json:"source"`
	ArtifactDigest string   `json:"artifact_digest,omitempty"`
	Egress         []string `json:"egress,omitempty"`
}

// renderDumpConfig is kept separate from the process CLI so tests can prove
// stdout is untouched when parsing/evaluation fails. Merge is policy's only
// merge path and Evaluate is policy's only permission projection.
func renderDumpConfig(base policy.Profile, overlays []policy.Profile, workspace string) ([]byte, error) {
	effective := base
	for _, overlay := range overlays {
		effective = policy.Merge(effective, overlay)
	}
	config, denial := policy.Evaluate(effective, policy.SpawnRequest{
		Workspace: workspace,
		Egress:    append([]string(nil), effective.Egress...),
		Depth:     0,
	})
	if denial != nil {
		return nil, denial
	}
	tree := dumpConfigTree{
		ProfileID:         redactString(config.ProfileID),
		FSScope:           sortedRedacted(config.FSScope),
		Workspace:         redactString(config.Workspace),
		Egress:            sortedRedacted(config.Egress),
		DeniedEgress:      sortedRedacted(config.DeniedEgress),
		AllowedExtensions: sortedRedacted(effective.AllowedExtensions),
		AllowedRegistries: sortedRedacted(effective.AllowedRegistries),
		Extensions:        make([]dumpExtension, 0, len(config.Extensions)),
		Budget:            config.Budget,
		Approval:          config.Approval,
		// A marker makes redaction observable without exposing whether any
		// particular credential was present. Values and host-only paths are
		// never serialized into this tree.
		Redaction: "<redacted>",
	}
	for _, extension := range config.Extensions {
		tree.Extensions = append(tree.Extensions, dumpExtension{
			Name: redactString(extension.Name), Version: redactString(extension.Version),
			Integrity: redactString(extension.Integrity), Source: redactString(extension.Source),
			Egress: sortedRedacted(extension.Egress),
		})
	}
	return marshalDeterministic(tree)
}

func marshalDeterministic(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sortedRedacted(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = redactString(value)
	}
	sort.Strings(out)
	return out
}

var sensitiveValuePattern = regexp.MustCompile(`(?i)(bearer\s+|(?:token|secret|password|authorization|api[_-]?key)\s*[:=]\s*)[^\s,;]+`)
var userInfoPattern = regexp.MustCompile(`(?i)(://)([^/@\s]+):([^/@\s]+)@`)

func redactString(value string) string {
	value = userInfoPattern.ReplaceAllString(value, `${1}<redacted>@`)
	return sensitiveValuePattern.ReplaceAllString(value, `${1}<redacted>`)
}
