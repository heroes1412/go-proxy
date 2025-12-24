//go:build linux
package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

func collectOSStats(lastAppTime *time.Duration, lastSysIdle, lastSysKernel, lastSysUser *uint64) (float64, float64, float64) {
	// 1. App CPU % (Tạm tính đơn giản cho Linux)
	appCPU := 0.1

	// 2. System RAM %
	sysRAM := 0.0
	f, err := os.Open("/proc/meminfo")
	if err == nil {
		scanner := bufio.NewScanner(f)
		var total, avail float64
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				total, _ = strconv.ParseFloat(fields[1], 64)
			}
			if strings.HasPrefix(line, "MemAvailable:") {
				fields := strings.Fields(line)
				avail, _ = strconv.ParseFloat(fields[1], 64)
			}
		}
		f.Close()
		if total > 0 {
			sysRAM = (total - avail) / total * 100
		}
	}

	// 3. System CPU % (Đọc từ /proc/stat)
	sysCPU := 0.0
	fstat, err := os.Open("/proc/stat")
	if err == nil {
		scanner := bufio.NewScanner(fstat)
		if scanner.Scan() {
			fields := strings.Fields(scanner.Text())[1:]
			var tot, idle uint64
			for i, v := range fields {
				val, _ := strconv.ParseUint(v, 10, 64)
				tot += val
				if i == 3 {
					idle = val
				}
			}
			if *lastSysIdle > 0 {
				diffTot := tot - (*lastSysIdle + *lastSysKernel + *lastSysUser)
				diffIdle := idle - *lastSysIdle
				if diffTot > 0 {
					sysCPU = (float64(diffTot-diffIdle) / float64(diffTot)) * 100
				}
			}
			// Lưu lại giá trị cho lần lấy mẫu sau
			*lastSysIdle = idle
			*lastSysKernel = tot - idle // Lưu phần còn lại vào kernel/user để tính delta
			*lastSysUser = 0
		}
		fstat.Close()
	}

	return appCPU, sysCPU, sysRAM
}