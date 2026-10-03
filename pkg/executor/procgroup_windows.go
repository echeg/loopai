//go:build windows

package executor

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processGroupCleanup owns a Windows Job Object so cancellation and normal
// cleanup terminate descendants too. If job setup fails, cancellation uses taskkill.
type processGroupCleanup struct {
	cmd      *exec.Cmd
	done     chan struct{}
	exited   chan struct{}
	once     sync.Once
	killOnce sync.Once
	err      error
	pipes    []io.Closer
	mu       sync.Mutex // serializes termination and job handle closure
	job      windows.Handle
	finished bool
}

// startProcessGroup keeps the initial thread suspended until job assignment, so
// even a fast launcher cannot create descendants outside the job.
func startProcessGroup(cmd *exec.Cmd, cancelCh <-chan struct{}, pipes ...io.Closer) (*processGroupCleanup, error) {
	return startProcessGroupWithAssign(cmd, cancelCh, windows.AssignProcessToJobObject, pipes...)
}

func startProcessGroupWithAssign(cmd *exec.Cmd, cancelCh <-chan struct{},
	assign func(windows.Handle, windows.Handle) error, pipes ...io.Closer) (*processGroupCleanup, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("open process for exit monitoring: %w", err)
	}
	pg := &processGroupCleanup{cmd: cmd, done: make(chan struct{}), exited: make(chan struct{}), pipes: pipes}
	// An unavailable job is not fatal: taskkill still provides tree cancellation.
	pg.job, _ = createProcessJob(cmd.Process.Pid, assign)
	go pg.watchProcessExit(process)
	go pg.watchForCancel(cancelCh)
	if err := resumeProcess(cmd.Process.Pid); err != nil {
		pg.killOnce.Do(pg.killProcess)
		_ = pg.Wait()
		return nil, fmt.Errorf("resume process: %w", err)
	}
	return pg, nil
}

// os/exec closes the initial thread handle. A suspended process has just that
// thread, which can be reopened through the documented Toolhelp API.
func resumeProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot threads: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return fmt.Errorf("open initial thread: %w", openErr)
		}
		_, err = windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		if err != nil {
			return fmt.Errorf("resume initial thread: %w", err)
		}
		return nil
	}
	return fmt.Errorf("find initial thread: %w", err)
}

// Close the job as soon as the launcher exits, independently of pipe draining.
// Otherwise an orphan holding stdout/stderr prevents callers from reaching Wait.
// Keep the read ends open here so callers can drain all buffered output.
func (pg *processGroupCleanup) watchProcessExit(process windows.Handle) {
	defer close(pg.exited)
	defer windows.CloseHandle(process)
	_, _ = windows.WaitForSingleObject(process, windows.INFINITE)
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.job != 0 {
		_ = windows.CloseHandle(pg.job)
		pg.job = 0
	}
}

// createProcessJob closes every acquired handle on failure; on success the caller
// owns the job. The short-lived process handle is needed only for assignment.
func createProcessJob(pid int, assign func(windows.Handle, windows.Handle) error) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil { //nolint:gosec // Windows API requires a pointer to this fixed-size structure
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("configure job: %w", err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("open process for job: %w", err)
	}
	defer windows.CloseHandle(process)
	if err = assign(job, process); err != nil {
		_ = windows.CloseHandle(job)
		return 0, fmt.Errorf("assign process to job: %w", err)
	}
	return job, nil
}

// watchForCancel terminates the tree and releases blocked output readers.
func (pg *processGroupCleanup) watchForCancel(cancelCh <-chan struct{}) {
	select {
	case <-cancelCh:
		pg.killOnce.Do(pg.killProcess)
	case <-pg.done:
	}
}

// killProcess terminates the job, falling back to a bounded taskkill invocation.
// Closing the read ends also unblocks callers if a process escaped containment.
func (pg *processGroupCleanup) killProcess() {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	if pg.finished {
		return
	}
	defer func() {
		for _, pipe := range pg.pipes {
			_ = pipe.Close()
		}
	}()
	if pg.job != 0 && windows.TerminateJobObject(pg.job, 1) == nil {
		return
	}
	process := pg.cmd.Process
	if process == nil || process.Pid <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(process.Pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if cmd.Run() != nil {
		_ = process.Kill()
	}
}

// Wait reaps the command and joins the exit watcher that closes its job.
// It is safe to call repeatedly. Closing is serialized with cancellation so a
// closed job handle is never used by a concurrent cancellation.
func (pg *processGroupCleanup) Wait() error {
	pg.once.Do(func() {
		pg.err = pg.cmd.Wait()
		<-pg.exited
		close(pg.done)
		pg.mu.Lock()
		pg.finished = true
		pg.mu.Unlock()
		if pg.err != nil {
			pg.err = fmt.Errorf("command wait: %w", pg.err)
		}
	})
	return pg.err
}
