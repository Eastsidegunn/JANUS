//go:build extensionsintegration

package main

// This is the T13 Linux gate. It deliberately uses a real HTTP server and a
// real rootless Podman container for provisioning; the unit-test fake runtime
// is not used as evidence here.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/policy"
	localworld "github.com/Eastsidegunn/JANUS/seams/world/local"
)

type extensionProvisionHandle struct {
	cmd  *exec.Cmd
	once sync.Once
}

func (h *extensionProvisionHandle) Wait(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = h.cmd.Process.Kill()
		return ctx.Err()
	}
}
func (h *extensionProvisionHandle) Cleanup(context.Context) error {
	h.once.Do(func() {
		if h.cmd.Process != nil {
			_ = h.cmd.Process.Kill()
		}
	})
	return nil
}

type podmanProvisionRuntime struct {
	image, url string
	mu         sync.Mutex
	output     string
}

func (r *podmanProvisionRuntime) Start(ctx context.Context, _ localworld.ProvisioningProfile, staging string, _ []gen.Extension) (localworld.ProvisioningHandle, error) {
	r.mu.Lock()
	r.output = filepath.Join(staging, "artifact")
	r.mu.Unlock()
	cmd := exec.CommandContext(ctx, "podman", "run", "--rm", "--pull=never", "--network", "host", "--volume", staging+":/out:rw", r.image, r.url, "/out/artifact")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &extensionProvisionHandle{cmd: cmd}, nil
}

func TestExtensionsIntegration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Fatalf("extensions-integration은 Linux 전용이며 skip 금지: %s", runtime.GOOS)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	info, err := exec.CommandContext(ctx, "podman", "info", "--format", "json").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(info), `"rootless":true`) || !strings.Contains(string(info), `"graphDriverName":"overlay"`) || !strings.Contains(string(info), `"Native Overlay Diff":"true"`) {
		t.Fatalf("rootless/native-overlay 전제 불충족: %s", info)
	}

	data := []byte("extension-artifact-v1\n")
	digest := "sha256:" + hex.EncodeToString(func() []byte { h := sha256.Sum256(data); return h[:] }())
	var tamper bool
	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := data
		if tamper {
			body = []byte("tampered\n")
		}
		_, _ = w.Write(body)
	})}
	ln, err := netListenLoopback()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go server.Serve(ln)
	root := t.TempDir()
	cache, err := localworld.NewContentCache(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	image := buildProvisionerImage(t, ctx)
	runtimeImpl := &podmanProvisionRuntime{image: image, url: "http://127.0.0.1:" + port(ln) + "/artifact"}
	ext := gen.Extension{Name: "demo", Version: "1.0.0", Integrity: digest, Source: "registry.test"}
	profile, _ := localworld.NewProvisioningProfile([]string{"registry.test"})
	prov, err := localworld.NewProvisioner(filepath.Join(root, "bundles"), cache, func(ctx context.Context, _ gen.Extension) ([]byte, error) {
		runtimeImpl.mu.Lock()
		p := runtimeImpl.output
		runtimeImpl.mu.Unlock()
		deadline := time.NewTimer(20 * time.Second)
		defer deadline.Stop()
		for {
			b, err := os.ReadFile(p)
			if err == nil {
				return b, nil
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-deadline.C:
				return nil, fmt.Errorf("provisioning container output: %w", err)
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1: real HTTP fetch in a one-shot Podman provisioning container, then
	// bundle sealing. The container is gone before callers can use the bundle.
	bundle, err := prov.ProvisionInContainer(ctx, runtimeImpl, profile, []gen.Extension{ext})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.IsZero() || len(bundle.Extensions()) != 1 || bundle.Extensions()[0].ArtifactDigest != digest {
		t.Fatalf("provision metadata=%+v", bundle.Extensions())
	}
	// 2: bytes tampering is fail-closed and leaves no second bundle.
	tamper = true
	badRoot := filepath.Join(root, "bad")
	badCache, _ := localworld.NewContentCache(filepath.Join(badRoot, "cache"))
	badProv, _ := localworld.NewProvisioner(filepath.Join(badRoot, "bundles"), badCache, func(context.Context, gen.Extension) ([]byte, error) { return []byte("tampered\n"), nil })
	if _, err := badProv.ProvisionWithProfile(ctx, profile, []gen.Extension{ext}); !errors.Is(err, localworld.ErrProvisioning) {
		t.Fatalf("tampered digest accepted: %v", err)
	}
	tamper = false
	// 6: policy denial occurs before any registry/runtime side effect.
	denied, _ := policy.Evaluate(policy.Profile{AllowedExtensions: nil, AllowedRegistries: nil, FSScope: []string{root}, Budget: gen.Budget{MaxDepth: 2}}, policy.SpawnRequest{Workspace: root, Extensions: []gen.Extension{ext}, Depth: 0})
	if len(denied.Extensions) != 0 {
		t.Fatal("unallowed extension passed policy")
	}
	// 7: cache hits are revalidated; corruption is never a successful hit.
	cachePath := filepath.Join(root, "cache", "sha256", strings.TrimPrefix(digest, "sha256:"))
	if err := os.WriteFile(cachePath, []byte("corrupt"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(ctx, digest); !errors.Is(err, localworld.ErrCacheCorrupt) {
		t.Fatalf("corrupt cache accepted: %v", err)
	}
	// The same real bundle is mounted read-only into the execution world. The
	// existing T10/T11 container gate then proves runtime registry hostname and
	// IP attempts are denied, explicit egress is allowed/audited, and metadata
	// carries the resolved extension set.
	artifacts := buildIntegrationArtifacts(t, ctx)
	runNormalIntegration(t, ctx, artifacts, bundle)
}

func buildProvisionerImage(t *testing.T, ctx context.Context) string {
	dir := t.TempDir()
	bin := filepath.Join(dir, "testprovisioner")
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", bin, "./seams/world/local/testprovisioner")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("build provisioner: %v %s", e, out)
	}
	ctxdir := filepath.Join(dir, "image")
	_ = os.Mkdir(ctxdir, 0o700)
	b, _ := os.ReadFile(bin)
	_ = os.WriteFile(filepath.Join(ctxdir, "testprovisioner"), b, 0o755)
	_ = os.WriteFile(filepath.Join(ctxdir, "Containerfile"), []byte("FROM scratch\nCOPY testprovisioner /testprovisioner\nENTRYPOINT [\"/testprovisioner\"]\n"), 0o600)
	tag := "localhost/hx-extension-provisioner:integration"
	if out, e := exec.CommandContext(ctx, "podman", "build", "--pull=never", "-t", tag, ctxdir).CombinedOutput(); e != nil {
		t.Fatalf("podman build: %v %s", e, out)
	}
	out, e := exec.CommandContext(ctx, "podman", "image", "inspect", "--format", "{{.Digest}}", tag).Output()
	if e != nil {
		t.Fatal(e)
	}
	digest := strings.TrimSpace(string(out))
	if !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("image digest=%q", digest)
	}
	t.Cleanup(func() { _ = exec.Command("podman", "image", "rm", "--force", tag).Run() })
	return tag + "@" + digest
}

func netListenLoopback() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
func port(l net.Listener) string               { return fmt.Sprint(l.Addr().(*net.TCPAddr).Port) }
