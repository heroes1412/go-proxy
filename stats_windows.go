//go:build windows
package main

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

func collectOSStats(lastAppTime *time.Duration, lastSysIdle, lastSysKernel, lastSysUser *uint64) (float64, float64, float64) {
	// 1. App CPU % (Lấy thời gian của riêng process này)
	var creationTime, exitTime, kernelTime, userTime syscall.Filetime
	handle, _ := syscall.GetCurrentProcess()
	syscall.GetProcessTimes(handle, &creationTime, &exitTime, &kernelTime, &userTime)
	totalAppTime := time.Duration(kernelTime.Nanoseconds() + userTime.Nanoseconds())
	
	appCPU := 0.0
	if *lastAppTime > 0 {
		// Delta thời gian CPU process / Delta thời gian thực * số CPU
		deltaApp := float64(totalAppTime - *lastAppTime)
		appCPU = (deltaApp / float64(2*time.Second*time.Duration(runtime.NumCPU()))) * 100
	}
	*lastAppTime = totalAppTime

	// 2. System RAM %
	var memStatus struct {
		Length     uint32
		MemoryLoad uint32
		TotalPhys  uint64
		AvailPhys  uint64
		TotalPage  uint64
		AvailPage  uint64
		TotalVurt  uint64
		AvailVurt  uint64
		Reserved   uint32
	}
	memStatus.Length = uint32(unsafe.Sizeof(memStatus))
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procMem := kernel32.NewProc("GlobalMemoryStatusEx")
	procMem.Call(uintptr(unsafe.Pointer(&memStatus)))
	sysRAM := float64(memStatus.MemoryLoad)

	// 3. System CPU % (Lấy thông số toàn hệ thống - Khớp Task Manager)
	var idleTime, sysKernelTime, sysUserTime syscall.Filetime
	procCPU := kernel32.NewProc("GetSystemTimes")
	procCPU.Call(uintptr(unsafe.Pointer(&idleTime)), uintptr(unsafe.Pointer(&sysKernelTime)), uintptr(unsafe.Pointer(&sysUserTime)))

	idle := uint64(idleTime.LowDateTime) | (uint64(idleTime.HighDateTime) << 32)
	kern := uint64(sysKernelTime.LowDateTime) | (uint64(sysKernelTime.HighDateTime) << 32)
	user := uint64(sysUserTime.LowDateTime) | (uint64(sysUserTime.HighDateTime) << 32)

	sysCPU := 0.0
	if *lastSysIdle > 0 {
		deltaIdle := idle - *lastSysIdle
		deltaKern := kern - *lastSysKernel
		deltaUser := user - *lastSysUser
		totalSys := deltaKern + deltaUser
		if totalSys > 0 {
			// CPU % = 1 - (Thời gian nghỉ / Tổng thời gian kernel + user)
			sysCPU = (1.0 - float64(deltaIdle)/float64(totalSys)) * 100
		}
	}
	*lastSysIdle, *lastSysKernel, *lastSysUser = idle, kern, user

	if sysCPU < 0 { sysCPU = 0 }
	if sysCPU > 100 { sysCPU = 100 }

	return appCPU, sysCPU, sysRAM
}