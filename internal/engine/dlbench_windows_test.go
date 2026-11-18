//go:build windows

package engine_test

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	psapi                = windows.NewLazySystemDLL("psapi.dll")
	procProcessMemory    = psapi.NewProc("GetProcessMemoryInfo")
	fsctlGetRetrievalPtr = uint32(0x00090073)
)

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// processPeakWorkingSet returns this process's peak working set in bytes.
func processPeakWorkingSet() int64 {
	var c processMemoryCounters
	c.cb = uint32(unsafe.Sizeof(c))
	r, _, _ := procProcessMemory.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&c)), uintptr(c.cb))
	if r == 0 {
		return 0
	}
	return int64(c.peakWorkingSetSize)
}

// extentCount returns how many separate runs of clusters hold the file, or -1.
func extentCount(path string) int {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return -1
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return -1
	}
	defer windows.CloseHandle(h)
	var vcn int64
	out := make([]byte, 1<<20)
	total := 0
	for {
		var returned uint32
		err := windows.DeviceIoControl(h, fsctlGetRetrievalPtr, (*byte)(unsafe.Pointer(&vcn)), 8, &out[0], uint32(len(out)), &returned, nil)
		if err != nil && err != windows.ERROR_MORE_DATA {
			return -1
		}
		// RETRIEVAL_POINTERS_BUFFER: ExtentCount uint32, pad, StartingVcn int64, then {NextVcn, Lcn} pairs.
		count := int(*(*uint32)(unsafe.Pointer(&out[0])))
		if count == 0 {
			return total
		}
		total += count
		if err == nil {
			return total
		}
		vcn = *(*int64)(unsafe.Pointer(&out[16+16*(count-1)]))
	}
}
