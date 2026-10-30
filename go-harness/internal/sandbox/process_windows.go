package sandbox

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"golang.org/x/sys/windows"
)

type windowsTree struct {
	job   windows.Handle
	owned []windowsOwnedProcess
}

type windowsOwnedProcess struct {
	pid     uint32
	handle  windows.Handle
	created uint64
}

func newProcessTree(cmd *exec.Cmd, limits harness.CommandLimits) (processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if limits.MemoryBytes > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(limits.MemoryBytes)
	}
	if limits.MaxProcesses > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.BasicLimitInformation.ActiveProcessLimit = uint32(limits.MaxProcesses)
	}
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	if limits.CPUPercent > 0 {
		cpu := struct{ Flags, Rate uint32 }{Flags: 0x1 | 0x4, Rate: limits.CPUPercent * 100}
		if _, err = windows.SetInformationJobObject(job, windows.JobObjectCpuRateControlInformation, uintptr(unsafe.Pointer(&cpu)), uint32(unsafe.Sizeof(cpu))); err != nil {
			windows.CloseHandle(job)
			return nil, err
		}
	}
	// The primary thread cannot spawn a descendant until it belongs to our Job.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW}
	return &windowsTree{job: job}, nil
}

func (t *windowsTree) afterStart(cmd *exec.Cmd) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION|windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	if err = windows.AssignProcessToJobObject(t.job, process); err != nil {
		windows.CloseHandle(process)
		return err
	}
	created, _, err := processLifetime(process)
	if err != nil {
		windows.CloseHandle(process)
		return err
	}
	// Keep the leader's identity after Cmd.Wait closes its handle. Packaged
	// execution aliases may activate brokered children outside this Job.
	t.owned = append(t.owned, windowsOwnedProcess{uint32(cmd.Process.Pid), process, created})
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(cmd.Process.Pid) {
			continue
		}
		thread, e := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if e != nil {
			return e
		}
		previous, e := windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if e != nil {
			return e
		}
		if previous > 0 {
			return nil
		}
	}
	return fmt.Errorf("cannot locate suspended command thread")
}

func (t *windowsTree) terminate() error {
	// Discover brokered children before the Job kills their parents. Retained
	// handles and creation/exit times prevent a stale parent PID from claiming
	// an unrelated process. Job termination remains the primary cleanup path.
	discoveryErr := t.discoverDescendants()
	// ActiveProcesses can reach zero just before the process handles become
	// signalled. Retain handles to known members and wait for actual exit too.
	members, enumerationErr := t.members()
	defer func() {
		for _, handle := range members {
			_ = windows.CloseHandle(handle)
		}
	}()
	jobErr := windows.TerminateJobObject(t.job, 1)
	deadline := time.Now().Add(cleanupTimeout)
	cleanupErr := t.terminateDescendants(deadline)
	if err := errors.Join(discoveryErr, enumerationErr, jobErr, cleanupErr); err != nil {
		return err
	}
	for _, handle := range members {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("Job member exit was not confirmed")
		}
		state, waitErr := windows.WaitForSingleObject(handle, uint32(remaining.Milliseconds()))
		if waitErr != nil {
			return waitErr
		}
		if state != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("Job member exit was not confirmed")
		}
	}
	for {
		accounting := struct {
			TotalUser, TotalKernel, PeriodUser, PeriodKernel int64
			Faults, Total, Active, Terminated                uint32
		}{}
		if err := windows.QueryInformationJobObject(t.job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
			return err
		}
		if accounting.Active == 0 {
			return enumerationErr
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Job still has active processes after termination")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processLifetime(handle windows.Handle) (created, exited uint64, err error) {
	var start, end, kernel, user windows.Filetime
	if err = windows.GetProcessTimes(handle, &start, &end, &kernel, &user); err != nil {
		return 0, 0, err
	}
	return uint64(start.HighDateTime)<<32 | uint64(start.LowDateTime),
		uint64(end.HighDateTime)<<32 | uint64(end.LowDateTime), nil
}

func (t *windowsTree) discoverDescendants() error {
	if len(t.owned) == 0 {
		return nil
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	children := make(map[uint32][]uint32)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		children[entry.ParentProcessID] = append(children[entry.ParentProcessID], entry.ProcessID)
	}
	if err != windows.ERROR_NO_MORE_FILES {
		return err
	}
	known := make(map[uint32]bool, len(t.owned))
	for _, process := range t.owned {
		known[process.pid] = true
	}
	for i := 0; i < len(t.owned); i++ {
		parent := t.owned[i]
		_, exited, err := processLifetime(parent.handle)
		if err != nil {
			return err
		}
		for _, pid := range children[parent.pid] {
			if known[pid] {
				continue
			}
			handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
			if err == windows.ERROR_INVALID_PARAMETER {
				continue // The process exited between the snapshot and OpenProcess.
			}
			if err != nil {
				return fmt.Errorf("open command descendant %d: %w", pid, err)
			}
			created, _, err := processLifetime(handle)
			if err != nil {
				windows.CloseHandle(handle)
				return err
			}
			if !withinParentLifetime(created, parent.created, exited) {
				windows.CloseHandle(handle)
				continue
			}
			if len(t.owned) >= 16384 {
				windows.CloseHandle(handle)
				return fmt.Errorf("command descendant enumeration exceeds safety bound")
			}
			t.owned = append(t.owned, windowsOwnedProcess{pid, handle, created})
			known[pid] = true
		}
	}
	return nil
}

func withinParentLifetime(child, parent, exited uint64) bool {
	return child >= parent && (exited == 0 || child <= exited)
}

func (t *windowsTree) terminateDescendants(deadline time.Time) error {
	for {
		// Children first; holding every handle also pins the identities until
		// the final snapshot accounts for children spawned during cleanup.
		for i := len(t.owned) - 1; i >= 0; i-- {
			handle := t.owned[i].handle
			state, err := windows.WaitForSingleObject(handle, 0)
			if err != nil {
				return err
			}
			if state == windows.WAIT_OBJECT_0 {
				continue
			}
			// A Job kill or natural exit may already be in progress. Windows
			// can deny a second TerminateProcess before the handle signals;
			// the bounded exit wait, not that race, decides confirmation.
			terminateErr := windows.TerminateProcess(handle, 1)
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return errors.Join(terminateErr, fmt.Errorf("command descendant exit was not confirmed"))
			}
			state, err = windows.WaitForSingleObject(handle, uint32(remaining.Milliseconds()))
			if err != nil {
				return err
			}
			if state != windows.WAIT_OBJECT_0 {
				return errors.Join(terminateErr, fmt.Errorf("command descendant exit was not confirmed"))
			}
		}
		previous := len(t.owned)
		if err := t.discoverDescendants(); err != nil {
			return err
		}
		if len(t.owned) == previous {
			return nil
		}
	}
}

func (t *windowsTree) members() ([]windows.Handle, error) {
	capacity := 32
	for capacity <= 16384 {
		buffer := make([]byte, 8+capacity*int(unsafe.Sizeof(uintptr(0))))
		err := windows.QueryInformationJobObject(t.job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&buffer[0])), uint32(len(buffer)), nil)
		if err == windows.ERROR_MORE_DATA {
			capacity *= 2
			continue
		}
		if err != nil {
			return nil, err
		}
		count := *(*uint32)(unsafe.Pointer(&buffer[4]))
		if int(count) > capacity {
			capacity *= 2
			continue
		}
		ids := unsafe.Slice((*uintptr)(unsafe.Pointer(&buffer[8])), int(count))
		var handles []windows.Handle
		for _, pid := range ids {
			handle, openErr := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
			if openErr == nil {
				handles = append(handles, handle)
			}
		}
		return handles, nil
	}
	return nil, fmt.Errorf("Job member enumeration exceeds safety bound")
}
func (t *windowsTree) close() error {
	var result error
	for _, process := range t.owned {
		result = errors.Join(result, windows.CloseHandle(process.handle))
	}
	t.owned = nil
	return errors.Join(result, windows.CloseHandle(t.job))
}
func defaultContainerUser() string { return "1000:1000" }
