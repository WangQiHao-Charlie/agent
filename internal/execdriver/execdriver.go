package execdriver

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "os/exec"
    "syscall"
    "time"

    "github.com/hackohio/agent/internal/util"
)

type Result struct {
    ExitCode       int
    StdoutTail     string
    StderrTail     string
    StdoutLastLine string
    TimedOut       bool
}

// Exec executes the command (cmd[0] binary and args) with given env and timeout.
// It never goes through a shell; it streams stdout/stderr into ring buffers for tails,
// tracks the last stdout line, and handles TERM->KILL on timeout.
func Exec(parent context.Context, executionID string, cmd []string, env []string, tailBytes int, termGrace time.Duration) (*Result, error) {
    if len(cmd) == 0 {
        return nil, errors.New("empty command")
    }
    c := exec.Command(cmd[0], cmd[1:]...) // never shell
    if len(env) > 0 {
        c.Env = append(c.Env, env...)
    }

    stdout, err := c.StdoutPipe()
    if err != nil {
        return nil, fmt.Errorf("stdout pipe: %w", err)
    }
    stderr, err := c.StderrPipe()
    if err != nil {
        return nil, fmt.Errorf("stderr pipe: %w", err)
    }

    outRB := util.NewRingBuffer(tailBytes)
    errRB := util.NewRingBuffer(tailBytes)
    var lastStdoutLine string

    if err := c.Start(); err != nil {
        return nil, fmt.Errorf("start: %w", err)
    }

    done := make(chan struct{})
    // Streamers
    go func() {
        stream(stdout, outRB, func(line string) { lastStdoutLine = line })
    }()
    go func() {
        stream(stderr, errRB, nil)
    }()

    var timedOut bool

    // Timeout handling (TERM then KILL)
    go func() {
        defer close(done)
        _ = c.Wait()
    }()

    select {
    case <-parent.Done():
        // Send TERM
        _ = c.Process.Signal(syscall.SIGTERM)
        // Wait for grace
        select {
        case <-done:
        case <-time.After(termGrace):
            _ = c.Process.Kill()
            timedOut = true
        }
    case <-done:
        // finished normally
    }

    // Extract exit code
    exitCode := -1
    if c.ProcessState != nil {
        if status, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok {
            exitCode = status.ExitStatus()
        }
    }

    res := &Result{
        ExitCode:       exitCode,
        StdoutTail:     string(outRB.Bytes()),
        StderrTail:     string(errRB.Bytes()),
        StdoutLastLine: lastStdoutLine,
        TimedOut:       timedOut,
    }
    return res, nil
}

func stream(r io.Reader, rb *util.RingBuffer, onLine func(string)) {
    scanner := bufio.NewScanner(r)
    buf := make([]byte, 0, 1024*1024)
    scanner.Buffer(buf, 1024*1024)
    for scanner.Scan() {
        b := scanner.Bytes()
        _, _ = rb.Write(append(b, '\n'))
        if onLine != nil {
            onLine(scanner.Text())
        }
    }
}

