//go:build t15smoke

package local

// NewBackendForT15Smoke enables the real Podman backend while the operator is
// running the T15 credential smoke on a macOS Podman machine. Podman CLI
// commands are executed against the machine's Linux VM; this entry point is
// compiled only with the human-run t15smoke tag and is not available to
// production surfaces or CI.
func NewBackendForT15Smoke(config Config) (*Backend, error) {
	return newBackend(config, execPodman{}, statDevice, startAuditBroker)
}
