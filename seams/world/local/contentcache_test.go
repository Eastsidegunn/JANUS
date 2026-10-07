package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
)

func TestContentCacheRevalidatesHitAndRejectsCorruption(t *testing.T) {
	root := t.TempDir()
	c, err := NewContentCache(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("verified extension")
	digest := hashBytes(data)
	if err := c.Put(context.Background(), digest, strings.NewReader(string(data))); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(context.Background(), digest)
	if err != nil || string(got) != string(data) {
		t.Fatalf("cache get = %q, %v", got, err)
	}
	p, _ := c.path(digest)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("tampered"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), digest); !errors.Is(err, ErrCacheCorrupt) {
		t.Fatalf("손상 hit가 fail-closed 아님: %v", err)
	}
}

func TestContentCachePutRejectsDigestMismatchImmediately(t *testing.T) {
	c, err := NewContentCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	digest := hashBytes([]byte("expected"))
	if err := c.Put(context.Background(), digest, strings.NewReader("wrong")); !errors.Is(err, ErrCacheCorrupt) {
		t.Fatalf("Put digest mismatch가 즉시 거부되지 않음: %v", err)
	}
	if _, err := c.Get(context.Background(), digest); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("실패한 Put이 cache entry를 남김: %v", err)
	}
}

func TestProvisionerSealsDeterministicBundleAndNoPartialOnFailure(t *testing.T) {
	root := t.TempDir()
	cache, err := NewContentCache(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvisioner(filepath.Join(root, "bundles"), cache, func(_ context.Context, ext gen.Extension) ([]byte, error) {
		return []byte(ext.Name + " bytes"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	makeExt := func(name string) gen.Extension {
		b := []byte(name + " bytes")
		return gen.Extension{Name: name, Version: "1.0.0", Integrity: hashBytes(b), Source: "registry.example"}
	}
	first, err := p.Provision(context.Background(), []gen.Extension{makeExt("zeta"), makeExt("alpha")})
	if err != nil {
		t.Fatal(err)
	}
	if first.Path() == "" || first.Digest() == "" || len(first.Extensions()) != 2 {
		t.Fatalf("bundle metadata 이상: %+v", first)
	}
	if _, err := os.Stat(filepath.Join(first.Path(), "manifest.json")); err != nil {
		t.Fatal(err)
	}
	bad, err := NewProvisioner(filepath.Join(root, "bad"), cache, func(context.Context, gen.Extension) ([]byte, error) { return []byte("wrong"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Provision(context.Background(), []gen.Extension{makeExt("broken")}); !errors.Is(err, ErrProvisioning) {
		t.Fatalf("digest mismatch가 거부되지 않음: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "bad"))
	if len(entries) != 0 {
		t.Fatalf("실패한 partial bundle 잔존: %v", entries)
	}
}

type fakeProvisionHandle struct {
	waited, cleaned   bool
	waitErr, cleanErr error
}

func (h *fakeProvisionHandle) Wait(context.Context) error { h.waited = true; return h.waitErr }
func (h *fakeProvisionHandle) Cleanup(context.Context) error {
	if !h.waited {
		return errors.New("cleanup before wait")
	}
	h.cleaned = true
	return h.cleanErr
}

type fakeProvisionRuntime struct {
	h       *fakeProvisionHandle
	started bool
	dir     string
	profile ProvisioningProfile
}

func (r *fakeProvisionRuntime) Start(_ context.Context, p ProvisioningProfile, dir string, _ []gen.Extension) (ProvisioningHandle, error) {
	r.started = true
	r.dir = dir
	r.profile = p
	return r.h, nil
}

func TestProvisionInContainerCleansBeforeReturningBundleAndProfileIsRegistryOnly(t *testing.T) {
	root := t.TempDir()
	cache, _ := NewContentCache(filepath.Join(root, "cache"))
	data := []byte("a bytes")
	ext := gen.Extension{Name: "a", Version: "1.0.0", Integrity: hashBytes(data), Source: "registry.example"}
	p, err := NewProvisioner(filepath.Join(root, "bundles"), cache, func(context.Context, gen.Extension) ([]byte, error) { return data, nil })
	if err != nil {
		t.Fatal(err)
	}
	profile, err := NewProvisioningProfile([]string{"REGISTRY.EXAMPLE."})
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeProvisionRuntime{h: &fakeProvisionHandle{}}
	bundle, err := p.ProvisionInContainer(context.Background(), rt, profile, []gen.Extension{ext})
	if err != nil {
		t.Fatal(err)
	}
	if !rt.started || !rt.h.waited || !rt.h.cleaned || bundle.Path() == "" {
		t.Fatalf("provision lifecycle 이상: started=%v waited=%v cleaned=%v bundle=%q", rt.started, rt.h.waited, rt.h.cleaned, bundle.Path())
	}
	if len(rt.profile.Registries()) != 1 || rt.profile.Registries()[0] != "registry.example" {
		t.Fatalf("registry profile canonicalization 이상: %v", rt.profile.Registries())
	}
	if _, err := p.ProvisionWithProfile(context.Background(), ProvisioningProfile{}, []gen.Extension{ext}); !errors.Is(err, ErrProvisioning) {
		t.Fatalf("빈 registry profile이 deny하지 않음: %v", err)
	}
}

func TestProvisionInContainerFailureDiscardsBundle(t *testing.T) {
	root := t.TempDir()
	cache, _ := NewContentCache(filepath.Join(root, "cache"))
	data := []byte("a bytes")
	ext := gen.Extension{Name: "a", Version: "1.0.0", Integrity: hashBytes(data), Source: "registry.example"}
	p, _ := NewProvisioner(filepath.Join(root, "bundles"), cache, func(context.Context, gen.Extension) ([]byte, error) { return data, nil })
	rt := &fakeProvisionRuntime{h: &fakeProvisionHandle{waitErr: errors.New("exit 7")}}
	profile := ProvisioningProfile{RegistryAllowlist: []string{"registry.example"}}
	if bundle, err := p.ProvisionInContainer(context.Background(), rt, profile, []gen.Extension{ext}); err == nil || !bundle.IsZero() {
		t.Fatalf("비정상 종료가 bundle을 노출함: bundle=%+v err=%v", bundle, err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "bundles"))
	if len(entries) != 0 {
		t.Fatalf("실패 후 bundle 잔존: %v", entries)
	}
}
