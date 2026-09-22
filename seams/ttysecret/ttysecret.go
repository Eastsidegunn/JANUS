//go:build linux || darwin

// Package ttysecret reads a single secret line directly from the controlling
// terminal with echo disabled — the Go equivalent of `read -s </dev/tty`
// (T20 요구조건 1). It is a seam because it needs an OS terminal primitive
// (golang.org/x/sys/unix), which the boundary lint confines to designated
// seams. The value is never taken from a file, argv, or an inherited
// environment variable, and the prompt is written to the tty itself (not
// stdout, which a surface uses for NDJSON control output).
package ttysecret

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Read prompts on and reads one hidden line from /dev/tty. The returned string
// is the only copy of the secret; the caller must wrap it in a host-only
// capability immediately and never persist it.
func Read(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("controlling terminal(/dev/tty) 열기 실패: %w", err)
	}
	defer tty.Close()

	fd := int(tty.Fd())
	before, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
	if err != nil {
		return "", fmt.Errorf("terminal 상태 읽기: %w", err)
	}
	noEcho := *before
	noEcho.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, ioctlWriteTermios, &noEcho); err != nil {
		return "", fmt.Errorf("terminal echo 비활성화: %w", err)
	}
	// Always restore the original terminal state, even on read error.
	defer unix.IoctlSetTermios(fd, ioctlWriteTermios, before)

	if _, err := tty.WriteString(prompt); err != nil {
		return "", err
	}
	reader := bufio.NewReader(tty)
	line, err := reader.ReadString('\n')
	// The user's Enter is not echoed (echo is off); emit a newline so the
	// terminal cursor advances past the prompt.
	_, _ = tty.WriteString("\n")
	if err != nil && line == "" {
		return "", fmt.Errorf("secret 읽기: %w", err)
	}
	secret := strings.TrimRight(line, "\r\n")
	if secret == "" {
		return "", fmt.Errorf("빈 secret은 허용되지 않음")
	}
	return secret, nil
}
