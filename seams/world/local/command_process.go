package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

type startedCommand interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Done() <-chan struct{}
	// ExitErr is valid only after Done closes.
	ExitErr() error
	Kill()
	ClosePipes()
}

type commandStarter interface {
	Start(context.Context, ...string) (startedCommand, error)
}

func (execPodman) Start(ctx context.Context, args ...string) (startedCommand, error) {
	return startCommand(ctx, podmanBinary, args...)
}

func startCommand(ctx context.Context, binary string, args ...string) (startedCommand, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%s %s stdin: %w", binary, strings.Join(args, " "), err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("%s %s stdout: %w", binary, strings.Join(args, " "), err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("%s %s stderr: %w", binary, strings.Join(args, " "), err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("%s %s start: %w", binary, strings.Join(args, " "), err)
	}
	p := &commandProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, done: make(chan struct{})}
	go p.reap()
	return p, nil
}

// commandProcess directly owns Process.Wait. Cmd.Wait is intentionally not
// used: exit observation must not wait for descendant-held stdout/stderr EOF.
type commandProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	done   chan struct{}

	mu        sync.Mutex
	exitErr   error
	exited    bool
	closeOnce sync.Once
}

func (p *commandProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *commandProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *commandProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *commandProcess) Done() <-chan struct{} { return p.done }
func (p *commandProcess) ExitErr() error        { p.mu.Lock(); defer p.mu.Unlock(); return p.exitErr }

func (p *commandProcess) reap() {
	state, err := p.cmd.Process.Wait()
	if err == nil && !state.Success() {
		err = &exec.ExitError{ProcessState: state}
	}
	p.mu.Lock()
	p.exitErr = err
	p.exited = true
	p.mu.Unlock()
	// The leader can exit while descendants retain pipe write ends. Kill the
	// remaining CLI process group so drain reaches real EOF without a grace
	// timeout or byte loss.
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	_ = p.stdin.Close()
	close(p.done)
}

func (p *commandProcess) Kill() {
	p.mu.Lock()
	exited := p.exited
	pid := p.cmd.Process.Pid
	p.mu.Unlock()
	if !exited {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func (p *commandProcess) ClosePipes() {
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		_ = p.stdout.Close()
		_ = p.stderr.Close()
	})
}

func commandExitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}
