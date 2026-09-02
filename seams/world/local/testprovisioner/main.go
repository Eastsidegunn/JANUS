// testprovisioner is a Linux integration helper. It performs a real HTTP
// fetch from the job-local artifact service and writes the bytes to the
// mounted staging directory. It is test-only and is never a production
// provisioning implementation.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: testprovisioner URL OUTPUT")
		os.Exit(2)
	}
	resp, err := http.Get(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		panic(fmt.Sprintf("artifact status=%d", resp.StatusCode))
	}
	if err := os.MkdirAll(filepath.Dir(os.Args[2]), 0o700); err != nil {
		panic(err)
	}
	f, err := os.OpenFile(os.Args[2], os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		panic(err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
}
