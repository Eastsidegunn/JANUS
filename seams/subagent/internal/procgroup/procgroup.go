// Package procgroup owns the lifecycle of a host-side child process and its
// process group. It is internal to the subagent seam so adapters and the
// generic §5.2 runner share the same reap, kill, pipe, and drain behavior.
package procgroup

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// Options describes a process launched in its own process group.
type Options struct {
	Command []string
	Dir     string
	Env     []string
	Stderr  io.Writer
}

// Process owns a child process, its process group, and both parent-side pipes.
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *os.File

	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
	exitErr   error // written before done is closed; read only after Done
}

// DrainResult keeps stream handling, scanning, and process exit failures
// separate so protocol-specific callers can preserve their error precedence.
type DrainResult struct {
	HandlerErr error
	ScanErr    error
	ExitErr    error
}

// Start launches a process in its own process group. Process.Wait is owned by
// the reaper goroutine and is called exactly once. Context cancellation kills
// the whole group rather than only the leader.
func Start(ctx context.Context, opts Options) (*Process, error) {
	if len(opts.Command) == 0 {
		return nil, fmt.Errorf("procgroup: command is empty")
	}
	cmd := exec.Command(opts.Command[0], opts.Command[1:]...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stderr = opts.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	stdoutFile, ok := stdout.(*os.File)
	if !ok {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("procgroup: stdout pipe is not *os.File (%T)", stdout)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdoutFile.Close()
		return nil, err
	}

	p := &Process{
		cmd: cmd, stdin: stdin, stdout: stdoutFile, done: make(chan struct{}),
	}
	go p.reap()
	go func() {
		select {
		case <-ctx.Done():
			p.Kill()
		case <-p.done:
		}
	}()
	return p, nil
}

// Done is closed by the reaper when leader termination has been observed.
// It is deliberately independent of stdout EOF and stream draining.
func (p *Process) Done() <-chan struct{} { return p.done }

// ExitErr reports the leader's exit result. It is valid only after Done closes.
func (p *Process) ExitErr() error { return p.exitErr }

// WriteLine serializes all writes to the single stdin file descriptor and
// appends exactly one NDJSON delimiter.
func (p *Process) WriteLine(line []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	b := make([]byte, 0, len(line)+1)
	b = append(b, line...)
	b = append(b, '\n')
	_, err := p.stdin.Write(b)
	return err
}

// Kill terminates the process group only while it has not been reaped. The
// Done guard prevents a late cancellation from signaling a reused PID/group.
func (p *Process) Kill() {
	select {
	case <-p.done:
		return
	default:
		killGroup(p.cmd)
	}
}

// DrainLines reads stdout through true EOF. The first handler error kills the
// process group and is retained; remaining buffered output is drained without
// invoking the handler. Parent-side pipes are always closed before return.
func (p *Process) DrainLines(maxBytes int, handler func([]byte) error) DrainResult {
	scanner := bufio.NewScanner(p.stdout)
	// 64KiB is only the initial allocation. maxBytes is the actual contract
	// limit, so valid lines above 64KiB remain accepted.
	scanner.Buffer(make([]byte, 0, 64*1024), maxBytes)

	var handlerErr error
	for scanner.Scan() {
		if handlerErr != nil {
			continue
		}
		if err := handler(scanner.Bytes()); err != nil {
			handlerErr = err
			p.Kill()
		}
	}
	scanErr := scanner.Err()
	<-p.done
	exitErr := p.ExitErr()
	p.ClosePipes()
	return DrainResult{HandlerErr: handlerErr, ScanErr: scanErr, ExitErr: exitErr}
}

// ClosePipes is an idempotent auxiliary cleanup operation. DrainLines invokes
// it automatically; callers use it only on failures before draining begins.
func (p *Process) ClosePipes() {
	p.closeOnce.Do(func() {
		p.writeMu.Lock()
		p.stdin.Close()
		p.writeMu.Unlock()
		p.stdout.Close()
	})
}

// StatStdin and StatStdout expose only pipe state for lifecycle regression
// tests; writes and reads remain owned by Process.
func (p *Process) StatStdin() (os.FileInfo, error) {
	f, ok := p.stdin.(*os.File)
	if !ok {
		return nil, fmt.Errorf("procgroup: stdin pipe is not *os.File (%T)", p.stdin)
	}
	return f.Stat()
}

func (p *Process) StatStdout() (os.FileInfo, error) { return p.stdout.Stat() }

func (p *Process) reap() {
	ps, err := p.cmd.Process.Wait()
	switch {
	case err != nil:
		p.exitErr = err
	case !ps.Success():
		p.exitErr = fmt.Errorf("exit: %s", ps.String())
	}
	// The leader is gone, but descendants may still own stdout. Kill the
	// remaining group before publishing Done so true EOF follows promptly.
	killGroup(p.cmd)
	close(p.done)
}

func killGroup(cmd *exec.Cmd) {
	if p := cmd.Process; p != nil {
		syscall.Kill(-p.Pid, syscall.SIGKILL)
		p.Kill()
	}
}
