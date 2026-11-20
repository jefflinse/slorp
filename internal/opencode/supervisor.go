package opencode

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const supervisorEnv = "_SLORP_OPENCODE_SUPERVISOR"
const supervisorPrivateEnv = "_SLORP_OPENCODE_PRIVATE"
const supervisorGrace = 2 * time.Second

// The private entry point also works in test executables. It is not a CLI mode:
// a dedicated process group and an inherited pipe are both required.
func init() {
	if os.Getenv(supervisorEnv) == "1" {
		os.Exit(supervise())
	}
}

func supervisedCommand(child *exec.Cmd, private string) (*exec.Cmd, *os.File, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(executable, append([]string{child.Path}, child.Args[1:]...)...)
	cmd.Dir, cmd.Env = child.Dir, append([]string{}, child.Env...)
	replaceEnv(cmd, supervisorEnv, "1")
	replaceEnv(cmd, supervisorPrivateEnv, private)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{read}
	cmd.Stdout, cmd.Stderr = child.Stdout, child.Stderr
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, nil, fmt.Errorf("starting OpenCode supervisor: %w", err)
	}
	// Only Slorp retains the write end. Neither the supervisor's child nor any
	// wrapper descendant can keep the parent-liveness pipe open.
	_ = read.Close()
	return cmd, write, nil
}

func supervise() int {
	pipe := os.NewFile(3, "slorp-parent")
	info, err := pipe.Stat()
	group, groupErr := syscall.Getpgid(0)
	private := os.Getenv(supervisorPrivateEnv)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 || groupErr != nil || group != os.Getpid() || len(os.Args) < 2 || !strings.HasPrefix(filepath.Base(private), "slorp-opencode-run-") {
		return 2
	}
	syscall.CloseOnExec(3)
	signal.Ignore(syscall.SIGPIPE)
	ended := make(chan os.Signal, 1)
	signal.Notify(ended, syscall.SIGTERM, os.Interrupt)
	parentGone := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, pipe); close(parentGone) }()
	child := exec.Command(os.Args[1], os.Args[2:]...)
	child.Env = os.Environ()
	replaceEnv(child, supervisorEnv, "")
	replaceEnv(child, supervisorPrivateEnv, "")
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		removePrivate(private)
		return 1
	}
	childDone := make(chan struct{})
	go func() { _ = child.Wait(); close(childDone) }()
	select {
	case <-parentGone:
	case <-ended:
	case <-childDone:
	}
	cleaned := make(chan struct{})
	go func() {
		if err := os.Remove(filepath.Join(private, "session.json")); err != nil && !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "removing managed control:", err)
		}
		time.Sleep(supervisorGrace)
		removePrivate(private)
		close(cleaned)
	}()
	// The supervisor remains the group leader through the final signal, so a
	// reaped wrapper PID can never become authority to kill an unrelated group.
	if err := syscall.Kill(-group, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		fmt.Fprintln(os.Stderr, "terminating managed group:", err)
	}
	select {
	case <-cleaned:
	case <-time.After(supervisorGrace + time.Second):
		fmt.Fprintln(os.Stderr, "private runtime cleanup exceeded its deadline")
	}
	if err := syscall.Kill(-group, syscall.SIGKILL); err != nil {
		fmt.Fprintln(os.Stderr, "killing managed group:", err)
		return 1
	}
	return 0
}
