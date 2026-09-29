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

type windowsTree struct{ job windows.Handle }

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
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	if err = windows.AssignProcessToJobObject(t.job, process); err != nil {
		return err
	}
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
	// ActiveProcesses can reach zero just before the process handles become
	// signalled. Retain handles to known members and wait for actual exit too.
	members, enumerationErr := t.members()
	defer func() {
		for _, handle := range members {
			_ = windows.CloseHandle(handle)
		}
	}()
	if err := windows.TerminateJobObject(t.job, 1); err != nil {
		return errors.Join(enumerationErr, err)
	}
	deadline := time.Now().Add(cleanupTimeout)
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
func (t *windowsTree) close() error { return windows.CloseHandle(t.job) }
func defaultContainerUser() string  { return "1000:1000" }
