package tgtest

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// logPoll is how often the bot's output is re-read while waiting for a line.
const logPoll = 200 * time.Millisecond

// Process is the bot under test, running as it does in production: a real
// process with a real environment, not a handler called in-process.
//
// Running the binary rather than calling into it is what makes startup ordering
// testable — whether commands are served before some slow initialisation
// finishes is a real and easily broken property.
type Process struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc

	mu  sync.Mutex
	out strings.Builder
}

// Start launches binary with env layered on top of the current environment.
func Start(binary string, env map[string]string) (*Process, error) {
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = cmd.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}

	p := &Process{cmd: cmd, cancel: cancel}
	cmd.Stdout = p
	cmd.Stderr = p

	err := cmd.Start()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}

	return p, nil
}

// Write collects output; it is called from the process's io goroutines.
func (p *Process) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.out.Write(b)
}

// Logs is everything the process has written so far.
//
// Worth asserting on and worth printing when a Telegram-side expectation fails:
// it distinguishes "the bot tried and failed" from "the bot never tried", which
// otherwise look identical from the chat.
func (p *Process) Logs() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.out.String()
}

// WaitForLog blocks until the process logs a line containing want.
func (p *Process) WaitForLog(want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		if strings.Contains(p.Logs(), want) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("never logged %q within %s\n--- output ---\n%s", want, timeout, p.Logs())
		}
		time.Sleep(logPoll)
	}
}

// Stop terminates the process and waits for it to exit.
func (p *Process) Stop() {
	p.cancel()
	_ = p.cmd.Wait()
}
