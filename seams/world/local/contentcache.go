package local

// This file contains the host-only content-addressed extension cache and the
// deterministic bundle sealer used by the provisioning stage. It deliberately
// has no registry or container-specific policy: callers must pass declarations
// already accepted by core/policy. A cache hit is never trusted without
// re-hashing its bytes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
	"github.com/Eastsidegunn/JANUS/core/world"
	"github.com/Eastsidegunn/JANUS/seams/world/local/egressproxy"
)

const (
	maxExtensionBytes = 64 << 20
	maxBundleBytes    = 256 << 20
)

var ErrCacheMiss = errors.New("world/local: extension cache miss")
var ErrCacheCorrupt = errors.New("world/local: extension cache integrity mismatch")
var ErrProvisioning = errors.New("world/local: provisioning failed")
var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ContentCache stores immutable bytes under cacheRoot/sha256/<hex>. The root
// is host-only state; no path is ever included in wire or durable metadata.
type ContentCache struct {
	root string
}

func NewContentCache(root string) (*ContentCache, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("world/local: cache root는 canonical 절대경로여야 함")
	}
	if err := os.MkdirAll(filepath.Join(root, "sha256"), 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	return &ContentCache{root: root}, nil
}

func (c *ContentCache) path(digest string) (string, error) {
	if c == nil || !digestRE.MatchString(digest) {
		return "", fmt.Errorf("%w: invalid digest", ErrCacheCorrupt)
	}
	return filepath.Join(c.root, "sha256", strings.TrimPrefix(digest, "sha256:")), nil
}

// Get reads and verifies a cache entry. A present but malformed entry is
// corruption, not a miss, so callers cannot silently use an unverified byte.
func (c *ContentCache) Get(ctx context.Context, digest string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := c.path(digest)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrCacheMiss
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read: %v", ErrCacheCorrupt, err)
	}
	if len(b) > maxExtensionBytes {
		return nil, fmt.Errorf("%w: entry too large", ErrCacheCorrupt)
	}
	if hashBytes(b) != digest {
		return nil, fmt.Errorf("%w: bytes digest mismatch", ErrCacheCorrupt)
	}
	return b, nil
}

// Put verifies bytes before atomically publishing an immutable cache entry.
// Existing entries are never overwritten, preventing a bad concurrent writer
// from replacing a valid artifact.
func (c *ContentCache) Put(ctx context.Context, digest string, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := c.path(digest)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".partial-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o400); err != nil {
		_ = tmp.Close()
		return err
	}
	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(tmp, h), r, maxExtensionBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		_ = tmp.Close()
		return err
	}
	if n > maxExtensionBytes {
		_ = tmp.Close()
		return fmt.Errorf("extension artifact exceeds %d bytes", maxExtensionBytes)
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		_ = tmp.Close()
		return fmt.Errorf("%w: downloaded bytes digest mismatch", ErrCacheCorrupt)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpName, p); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	return nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ArtifactFetcher is implemented by the provisioning container adapter. It
// returns the artifact bytes obtained through that container's registry-only
// proxy; it must not write directly into the host bundle.
type ArtifactFetcher func(context.Context, gen.Extension) ([]byte, error)

// ProvisioningRuntime is the narrow lifecycle seam for the one-shot
// registry-only container. Start must not expose a workspace; Cleanup must
// acknowledge removal of the container, proxy socket, and network before it
// returns. The execution world is created only after this handle is gone.
type ProvisioningRuntime interface {
	Start(context.Context, ProvisioningProfile, string, []gen.Extension) (ProvisioningHandle, error)
}

type ProvisioningHandle interface {
	Wait(context.Context) error
	Cleanup(context.Context) error
}

// ProvisioningProfile is intentionally separate from the execution egress
// profile. Its allowlist contains only registry hosts and is never copied to
// a world Backend or execution proxy.
type ProvisioningProfile struct{ RegistryAllowlist []string }

func NewProvisioningProfile(registries []string) (ProvisioningProfile, error) {
	allow, err := egressproxy.NormalizeAllowlist(registries)
	if err != nil {
		return ProvisioningProfile{}, err
	}
	return ProvisioningProfile{RegistryAllowlist: append([]string(nil), allow...)}, nil
}
func (p ProvisioningProfile) Registries() []string {
	return append([]string(nil), p.RegistryAllowlist...)
}

func (p ProvisioningProfile) allows(source string) bool {
	for _, allowed := range p.RegistryAllowlist {
		if source == allowed {
			return true
		}
	}
	return false
}

// Provisioner seals one verified bundle. Fetch is intentionally injected so
// unit tests can exercise failure and cache paths without networking; the
// Linux integration stage supplies the real one-shot Podman container.
type Provisioner struct {
	cache *ContentCache
	root  string
	fetch ArtifactFetcher
}

func NewProvisioner(root string, cache *ContentCache, fetch ArtifactFetcher) (*Provisioner, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || cache == nil || fetch == nil {
		return nil, fmt.Errorf("world/local: provisioning 설정 위반")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Provisioner{cache: cache, root: root, fetch: fetch}, nil
}

// Provision validates, fetches/revalidates, and atomically seals a bundle. No
// bundle is returned on any failure; the temporary directory is removed.
func (p *Provisioner) Provision(ctx context.Context, declarations []gen.Extension) (world.ExtensionBundle, error) {
	return p.provision(ctx, declarations, nil)
}

// ProvisionWithProfile applies the registry-only provisioning profile before
// any cache write or fetch. An empty profile is deny-all, matching policy's
// fail-closed omitted-list semantics.
func (p *Provisioner) ProvisionWithProfile(ctx context.Context, profile ProvisioningProfile, declarations []gen.Extension) (world.ExtensionBundle, error) {
	return p.provision(ctx, declarations, &profile)
}

// ProvisionInContainer runs the fetch/seal operation while a registry-only
// provisioning container is alive, then waits for its cleanup ACK. Any wait or
// cleanup error discards the bundle and returns no execution capability.
func (p *Provisioner) ProvisionInContainer(ctx context.Context, runtime ProvisioningRuntime, profile ProvisioningProfile, declarations []gen.Extension) (bundle world.ExtensionBundle, err error) {
	if runtime == nil {
		return world.ExtensionBundle{}, fmt.Errorf("%w: provisioning runtime 없음", ErrProvisioning)
	}
	staging, err := os.MkdirTemp(p.root, ".provision-stage-")
	if err != nil {
		return world.ExtensionBundle{}, err
	}
	defer os.RemoveAll(staging)
	handle, err := runtime.Start(ctx, profile, staging, append([]gen.Extension(nil), declarations...))
	if err != nil {
		return world.ExtensionBundle{}, fmt.Errorf("%w: container start: %v", ErrProvisioning, err)
	}
	if handle == nil {
		return world.ExtensionBundle{}, fmt.Errorf("%w: nil provisioning handle", ErrProvisioning)
	}
	cleanupCtx, cancel := cleanupContext(ctx)
	defer cancel()
	defer func() {
		if err != nil && bundle.Path() != "" {
			_ = os.RemoveAll(bundle.Path())
			bundle = world.ExtensionBundle{}
		}
	}()
	defer func() {
		cleanupErr := handle.Cleanup(cleanupCtx)
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("%w: container cleanup: %v", ErrProvisioning, cleanupErr))
			if bundle.Path() != "" {
				_ = os.RemoveAll(bundle.Path())
			}
			bundle = world.ExtensionBundle{}
		}
	}()
	bundle, err = p.provision(ctx, declarations, &profile)
	if err != nil {
		return world.ExtensionBundle{}, err
	}
	if waitErr := handle.Wait(ctx); waitErr != nil {
		_ = os.RemoveAll(bundle.Path())
		bundle = world.ExtensionBundle{}
		err = fmt.Errorf("%w: container exit: %v", ErrProvisioning, waitErr)
		return world.ExtensionBundle{}, err
	}
	return bundle, nil
}

func (p *Provisioner) provision(ctx context.Context, declarations []gen.Extension, profile *ProvisioningProfile) (world.ExtensionBundle, error) {
	if len(declarations) == 0 {
		return world.ExtensionBundle{}, fmt.Errorf("%w: declarations empty", ErrProvisioning)
	}
	exts := make([]gen.Extension, len(declarations))
	seen := map[string]bool{}
	for i, raw := range declarations {
		ext, err := policy.NormalizeExtension(raw)
		if err != nil {
			return world.ExtensionBundle{}, fmt.Errorf("%w: %v", ErrProvisioning, err)
		}
		key := strings.Join([]string{ext.Name, ext.Version, ext.Source, ext.Integrity}, "\x00")
		if seen[key] {
			return world.ExtensionBundle{}, fmt.Errorf("%w: duplicate extension", ErrProvisioning)
		}
		if profile != nil && !profile.allows(ext.Source) {
			return world.ExtensionBundle{}, fmt.Errorf("%w: registry %s가 provisioning profile 밖", ErrProvisioning, ext.Source)
		}
		seen[key] = true
		exts[i] = ext
	}
	sort.Slice(exts, func(i, j int) bool { return extensionKey(exts[i]) < extensionKey(exts[j]) })
	work, err := os.MkdirTemp(p.root, ".bundle-")
	if err != nil {
		return world.ExtensionBundle{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(work)
		}
	}()
	if err := os.Chmod(work, 0o700); err != nil {
		return world.ExtensionBundle{}, err
	}
	resolved := make([]gen.SubagentSpawnExtension, 0, len(exts))
	var total int64
	for _, ext := range exts {
		if err := ctx.Err(); err != nil {
			return world.ExtensionBundle{}, err
		}
		bytes, err := p.cache.Get(ctx, ext.Integrity)
		if errors.Is(err, ErrCacheMiss) {
			bytes, err = p.fetch(ctx, ext)
			if err == nil {
				err = p.cache.Put(ctx, ext.Integrity, bytesReader(bytes))
			}
			if err == nil {
				bytes, err = p.cache.Get(ctx, ext.Integrity)
			}
		}
		if err != nil {
			return world.ExtensionBundle{}, fmt.Errorf("%w: %s: %v", ErrProvisioning, ext.Name, err)
		}
		total += int64(len(bytes))
		if total > maxBundleBytes {
			return world.ExtensionBundle{}, fmt.Errorf("%w: bundle exceeds size limit", ErrProvisioning)
		}
		name := safeArtifactName(ext.Name + "@" + ext.Version)
		path := filepath.Join(work, name)
		if err := os.WriteFile(path, bytes, 0o444); err != nil {
			return world.ExtensionBundle{}, err
		}
		resolved = append(resolved, gen.SubagentSpawnExtension{Name: ext.Name, Version: ext.Version, Integrity: ext.Integrity, Source: ext.Source, ArtifactDigest: hashBytes(bytes)})
	}
	manifest := struct {
		Extensions []gen.SubagentSpawnExtension `json:"extensions"`
	}{resolved}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(work, "manifest.json"), mb, 0o444); err != nil {
		return world.ExtensionBundle{}, err
	}
	// Seal the bundle contents before it can be handed to an execution world.
	// The directory remains host-cleanable (0700); the execution container
	// receives the whole tree read-only and cannot use host ownership as a trust
	// boundary.
	bundleDigest, err := hashDirectory(work)
	if err != nil {
		return world.ExtensionBundle{}, err
	}
	sealed := filepath.Join(p.root, "bundle-"+strings.TrimPrefix(bundleDigest, "sha256:"))
	if err := os.Rename(work, sealed); err != nil {
		return world.ExtensionBundle{}, err
	}
	cleanup = false
	return world.NewExtensionBundle(sealed, bundleDigest, resolved), nil
}

type byteReader struct {
	b   []byte
	off int
}

func bytesReader(b []byte) io.Reader { return &byteReader{b: b} }
func (r *byteReader) Read(p []byte) (int, error) {
	if r.off == len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

func extensionKey(e gen.Extension) string {
	return strings.Join([]string{e.Name, e.Version, e.Source, e.Integrity}, "\x00")
}
func safeArtifactName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
	return s
}

func hashDirectory(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(h, e.Name()+"\x00")
		_, _ = h.Write(b)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
