//go:build windows

package childproc

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// KillChildrenOnExit puts this process in a job object that kills every
// process in it when its last handle closes, which happens when this
// process ends, however it ends. Children started afterwards (ffmpeg,
// whisper.cpp) join the job, so a crash or kill of the daemon never leaves
// a transcription running.
func KillChildrenOnExit() error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("configure job object: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("join job object: %w", err)
	}
	// The handle stays open for the life of the process: closing it would
	// kill the process itself.
	return nil
}
