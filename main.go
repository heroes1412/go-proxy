package main

import (
	"bufio"
	"context"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// --- DATA STRUCTURES ---

type IPStat struct {
	lastReset time.Time
	count     int32
}

type Backend struct {
	Address     string
	Alive       bool
	ActiveConns int64
	mux         sync.RWMutex
}

type ProxyService struct {
	Name         string
	Port         string
	Protocol     string
	Backends     []*Backend
	Index        uint64
	ErrorMsg     string
	Listener     net.Listener
	quit         chan bool
	CurrentConns int64
	IPMap        sync.Map
	PenaltyBox   sync.Map
	RateMap      sync.Map
	mux          sync.RWMutex
	BypassLimits bool // Field cho tham số bypass optional
}

type IdleTimeoutConn struct {
	net.Conn
	Timeout time.Duration
}

func (c *IdleTimeoutConn) Read(b []byte) (int, error) {
	c.Conn.SetDeadline(time.Now().Add(c.Timeout))
	return c.Conn.Read(b)
}
func (c *IdleTimeoutConn) Write(b []byte) (int, error) {
	c.Conn.SetDeadline(time.Now().Add(c.Timeout))
	return c.Conn.Write(b)
}

// --- GLOBAL VARIABLES ---

var (
	MaxGlobalConns        int64         = 2000
	MaxConnsPerIP         int32         = 50
	idleTimeoutHTTPNS      int64         = int64(60 * time.Second)
	idleTimeoutTCPNS       int64         = int64(660 * time.Second)
	connRateLimit          int32         = 10
	connRateWindowNS       int64         = int64(5 * time.Second)
	penaltyDurationNS      int64         = int64(300 * time.Second)
	healthCheckIntervalNS  int64         = int64(3 * time.Second)
	CurrentMonitorPort     string        = "8080"
	monitorServer          *http.Server
	globalMux              sync.Mutex
	lastModTime            time.Time
	configFileName         = "config.txt"
	services               = make(map[string]*ProxyService)
	mainTmpl               *template.Template
	totalActiveConns       int64

	appCPUUsage uint64
	sysCPUUsage uint64
	sysRAMUsage uint64

	logMutex sync.Mutex 
)

var bufPool = sync.Pool{
	New: func() interface{} { return make([]byte, 32*1024) },
}

// --- LOGGING SYSTEM ---

func writeToLogFile(fileName, level, format string, v ...interface{}) {
	logMutex.Lock()
	defer logMutex.Unlock()

	os.MkdirAll("logs", 0755)
	path := fmt.Sprintf("logs/%s", fileName)

	if info, err := os.Stat(path); err == nil {
		if info.Size() > 1024*1024 { 
			os.OpenFile(path, os.O_TRUNC|os.O_WRONLY, 0644)
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	t := time.Now().Format("15:04:05 02/01/2006")
	msg := fmt.Sprintf("[%s] [%s] "+format+"\n", append([]interface{}{t, level}, v...)...)

	fmt.Print(msg)
	f.WriteString(msg)
}

func infoLog(format string, v ...interface{}) { writeToLogFile("system.log", "INFO", format, v...) }
func errLog(format string, v ...interface{})  { writeToLogFile("system.log", "ERROR", format, v...) }

func (s *ProxyService) warnLog(format string, v ...interface{}) {
	writeToLogFile(fmt.Sprintf("port_%s.log", s.Port), "WARN", format, v...)
}
func (s *ProxyService) infoLog(format string, v ...interface{}) {
	writeToLogFile(fmt.Sprintf("port_%s.log", s.Port), "INFO", format, v...)
}

// --- CORE LOGIC ---

func main() {
	fmt.Println("===========================================")
	fmt.Println("    TCP PROXY - LOCAL LIMITS & LOGGING")
	fmt.Println("===========================================")

	initTemplate()
	reloadConfig(true)
	go watchConfigFile()
	
	startMonitorServer(CurrentMonitorPort)
	select {}
}

func reloadConfig(isInitial bool) {
	file, err := os.Open(configFileName)
	if err != nil { return }
	defer file.Close()
	stat, _ := file.Stat()
	lastModTime = stat.ModTime()
	globalMux.Lock()
	defer globalMux.Unlock()
	scanner := bufio.NewScanner(file)
	section := ""
	newPorts := make(map[string]bool)
	oldMPort := CurrentMonitorPort
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") { continue }
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") { section = strings.ToLower(line); continue }
		if section == "[global]" {
			parts := strings.Split(line, "=")
			if len(parts) != 2 { continue }
			key, valStr := strings.TrimSpace(parts[0]), strings.TrimSpace(strings.Split(parts[1], "//")[0])
			if key == "MonitorPort" { CurrentMonitorPort = valStr; continue }
			val, _ := strconv.ParseInt(valStr, 10, 64)
			switch key {
			case "MaxTotalConnsPerService": atomic.StoreInt64(&MaxGlobalConns, val)
			case "MaxConnsPerIP":           atomic.StoreInt32(&MaxConnsPerIP, int32(val))
			case "IdleTimeoutHTTPInSec":     atomic.StoreInt64(&idleTimeoutHTTPNS, int64(time.Duration(val)*time.Second))
			case "IdleTimeoutTCPInSec":      atomic.StoreInt64(&idleTimeoutTCPNS, int64(time.Duration(val)*time.Second))
			case "ConnRateLimit":           atomic.StoreInt32(&connRateLimit, int32(val))
			case "ConnRateWindowInSec":     atomic.StoreInt64(&connRateWindowNS, int64(time.Duration(val)*time.Second))
			case "PenaltyDurationInSec":    atomic.StoreInt64(&penaltyDurationNS, int64(time.Duration(val)*time.Second))
			case "HealthCheckInternal":     atomic.StoreInt64(&healthCheckIntervalNS, int64(time.Duration(val)*time.Second))
			}
		} else if section == "[proxy]" {
			parts := strings.Split(line, "|")
			if len(parts) < 3 { continue }
			name, port, bAddr := parts[0], parts[1], parts[2]
			proto := "tcp"
			if len(parts) >= 4 { proto = strings.ToLower(strings.TrimSpace(parts[3])) }
			
			// Tham số thứ 5 là optional bypass
			isBypass := false
			if len(parts) >= 5 && strings.TrimSpace(strings.ToLower(parts[4])) == "bypass" {
				isBypass = true
			}

			newPorts[port] = true
			if svc, exists := services[port]; exists {
				svc.mux.Lock()
				svc.Name, svc.Protocol, svc.BypassLimits = name, proto, isBypass
				oMap := make(map[string]*Backend)
				for _, b := range svc.Backends { oMap[b.Address] = b }
				var updated []*Backend
				for _, a := range strings.Split(bAddr, ",") {
					addr := strings.TrimSpace(a)
					if oldB, ok := oMap[addr]; ok { updated = append(updated, oldB) } else { updated = append(updated, &Backend{Address: addr, Alive: false}) }
				}
				svc.Backends = updated
				if svc.ErrorMsg != "" { go svc.startProxy() }
				svc.mux.Unlock()
			} else {
				newSvc := &ProxyService{Name: name, Port: port, Protocol: proto, BypassLimits: isBypass, quit: make(chan bool)}
				for _, a := range strings.Split(bAddr, ",") { newSvc.Backends = append(newSvc.Backends, &Backend{Address: strings.TrimSpace(a), Alive: false}) }
				services[port] = newSvc
				go newSvc.startProxy()
				go newSvc.startHealthCheck()
			}
		}
	}
	if CurrentMonitorPort != oldMPort && !isInitial { go startMonitorServer(CurrentMonitorPort) }
	for p, s := range services { if !newPorts[p] { s.stop(); delete(services, p) } }
	if !isInitial { infoLog("Configuration reloaded") }
}

func (s *ProxyService) startProxy() {
	s.mux.Lock()
	if s.Listener != nil { s.mux.Unlock(); return }
	s.mux.Unlock()
	s.checkBackendsOnce()
	ln, err := net.Listen("tcp4", "0.0.0.0:"+s.Port)
	if err != nil { s.mux.Lock(); s.ErrorMsg = "Port in use"; s.mux.Unlock(); return }
	s.mux.Lock(); s.Listener = ln; s.ErrorMsg = ""; s.mux.Unlock()
	s.infoLog("Proxy %s running on port %s (Protocol: %s, Bypass: %v)", s.Name, s.Port, s.Protocol, s.BypassLimits)

	for {
		conn, err := ln.Accept()
		if err != nil { return }
		ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())

		var activeIPCounter *int32

		if !s.BypassLimits {
			// 1. LOCAL PENALTY BOX
			if ban, blocked := s.PenaltyBox.Load(ip); blocked {
				if time.Now().Before(ban.(time.Time)) {
					s.warnLog("[%s] Block IP %s: Still in Local Penalty Box", s.Name, ip)
					conn.Close(); continue
				}
				s.PenaltyBox.Delete(ip)
			}

			// 2. LOCAL RATE LIMIT
			v, _ := s.RateMap.LoadOrStore(ip, &IPStat{lastReset: time.Now()})
			stat := v.(*IPStat)
			window := time.Duration(atomic.LoadInt64(&connRateWindowNS))
			if time.Since(stat.lastReset) > window {
				atomic.StoreInt32(&stat.count, 1); stat.lastReset = time.Now()
			} else {
				if atomic.AddInt32(&stat.count, 1) > atomic.LoadInt32(&connRateLimit) {
					p := time.Duration(atomic.LoadInt64(&penaltyDurationNS))
					s.warnLog("[%s] Block IP %s: Local Rate limit exceeded", s.Name, ip)
					s.PenaltyBox.Store(ip, time.Now().Add(p))
					atomic.StoreInt32(&stat.count, 0)
					stat.lastReset = time.Now()
					conn.Close(); continue
				}
			}

			// 3. LOCAL MAX CONNS PER IP
			valIP, _ := s.IPMap.LoadOrStore(ip, new(int32))
			activeIPCounter = valIP.(*int32)
			if atomic.AddInt32(activeIPCounter, 1) > atomic.LoadInt32(&MaxConnsPerIP) {
				s.warnLog("[%s] Block IP %s: Reached Local MaxConnsPerIP (%d)", s.Name, ip, atomic.LoadInt32(&MaxConnsPerIP))
				atomic.AddInt32(activeIPCounter, -1)
				conn.Close(); continue
			}
		}

		// 4. GLOBAL LIMIT
		if atomic.LoadInt64(&totalActiveConns) >= atomic.LoadInt64(&MaxGlobalConns) {
			errLog("Block connection: Global limit reached (%d)", atomic.LoadInt64(&MaxGlobalConns))
			if activeIPCounter != nil { atomic.AddInt32(activeIPCounter, -1) }
			conn.Close(); continue
		}

		atomic.AddInt64(&totalActiveConns, 1)
		atomic.AddInt64(&s.CurrentConns, 1)
		go s.handleConnection(conn, activeIPCounter)
	}
}

func (s *ProxyService) handleConnection(clientConn net.Conn, ipCounter *int32) {
	defer func() {
		if r := recover(); r != nil { s.warnLog("Panic: %v", r) }
		clientConn.Close()
		atomic.AddInt64(&totalActiveConns, -1)
		atomic.AddInt64(&s.CurrentConns, -1)
		if ipCounter != nil { atomic.AddInt32(ipCounter, -1) }
	}()
	s.mux.RLock(); proto, backend := s.Protocol, s.getNextAliveBackend(); s.mux.RUnlock()
	if backend == nil { return }
	targetConn, err := net.DialTimeout("tcp", backend.Address, 5*time.Second)
	if err != nil { return }
	defer targetConn.Close()
	atomic.AddInt64(&backend.ActiveConns, 1); defer atomic.AddInt64(&backend.ActiveConns, -1)
	var timeout time.Duration
	if proto == "http" { timeout = time.Duration(atomic.LoadInt64(&idleTimeoutHTTPNS)) } else { timeout = time.Duration(atomic.LoadInt64(&idleTimeoutTCPNS)) }
	clientConn.SetDeadline(time.Now().Add(timeout)); targetConn.SetDeadline(time.Now().Add(timeout))
	done := make(chan bool, 2); cp := func(dst io.Writer, src io.Reader) {
		buf := bufPool.Get().([]byte); defer bufPool.Put(buf); io.CopyBuffer(dst, src, buf); done <- true
	}
	go cp(targetConn, &IdleTimeoutConn{clientConn, timeout}); go cp(clientConn, &IdleTimeoutConn{targetConn, timeout}); <-done
}

// --- HELPERS ---

func (s *ProxyService) checkBackendsOnce() {
	s.mux.RLock(); backends := s.Backends; s.mux.RUnlock()
	for _, b := range backends {
		conn, err := net.DialTimeout("tcp", b.Address, 1*time.Second)
		isAliveNow := (err == nil)

		b.mux.Lock()
		if b.Alive != isAliveNow {
			if isAliveNow {
				s.infoLog("[%s] BACKEND ONLINE: %s", s.Name, b.Address)
			} else {
				s.warnLog("[%s] BACKEND OFFLINE: %s", s.Name, b.Address)
			}
			b.Alive = isAliveNow
		}
		if err == nil { conn.Close() }
		b.mux.Unlock()
	}
}
func (s *ProxyService) startHealthCheck() {
	for {
		interval := time.Duration(atomic.LoadInt64(&healthCheckIntervalNS))
		if interval < 1*time.Second { interval = 3 * time.Second }; time.Sleep(interval)
		select { case <-s.quit: return; default: s.checkBackendsOnce() }
	}
}
func (s *ProxyService) stop() {
	s.mux.Lock(); defer s.mux.Unlock()
	if s.Listener != nil { s.Listener.Close(); s.Listener = nil }
	if s.quit != nil { select { case <-s.quit: default: close(s.quit) } }
}
func (s *ProxyService) getNextAliveBackend() *Backend {
	s.mux.RLock(); defer s.mux.RUnlock()
	if len(s.Backends) == 0 { return nil }
	for i := 0; i < len(s.Backends); i++ {
		idx := atomic.AddUint64(&s.Index, 1) % uint64(len(s.Backends))
		b := s.Backends[idx]; b.mux.RLock(); alive := b.Alive; b.mux.RUnlock()
		if alive { return b }
	}
	return nil
}
func watchConfigFile() {
	for {
		time.Sleep(2 * time.Second); info, err := os.Stat(configFileName)
		if err == nil { globalMux.Lock(); if info.ModTime().After(lastModTime) { globalMux.Unlock(); reloadConfig(false) } else { globalMux.Unlock() } }
	}
}

func startMonitorServer(port string) {
	if monitorServer != nil { monitorServer.Shutdown(context.Background()) }
	go func() {
		var lastAppTime time.Duration; var lastSysIdle, lastSysKernel, lastSysUser uint64
		for {
			// Hàm này được định nghĩa trong stats_windows.go hoặc stats_linux.go
			aCPU, sCPU, sRAM := collectOSStats(&lastAppTime, &lastSysIdle, &lastSysKernel, &lastSysUser)
			atomic.StoreUint64(&appCPUUsage, *(*uint64)(unsafe.Pointer(&aCPU))); atomic.StoreUint64(&sysCPUUsage, *(*uint64)(unsafe.Pointer(&sCPU)))
			atomic.StoreUint64(&sysRAMUsage, *(*uint64)(unsafe.Pointer(&sRAM))); time.Sleep(2 * time.Second)
		}
	}()
	mux := http.NewServeMux(); mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		globalMux.Lock(); defer globalMux.Unlock(); var m runtime.MemStats; runtime.ReadMemStats(&m)
		aCPU := *(*float64)(unsafe.Pointer(&appCPUUsage)); sCPU := *(*float64)(unsafe.Pointer(&sysCPUUsage)); sRAM := *(*float64)(unsafe.Pointer(&sysRAMUsage))
		data := struct { Services map[string]*ProxyService; MaxGlobal int64; TotalActive int64; AppCPU float64; AppRAM float64; SysCPU float64; SysRAM float64 }{services, atomic.LoadInt64(&MaxGlobalConns), atomic.LoadInt64(&totalActiveConns), aCPU, float64(m.Alloc) / 1024 / 1024, sCPU, sRAM}
		mainTmpl.Execute(w, data)
	}); monitorServer = &http.Server{Addr: ":" + port, Handler: mux}; log.Fatal(monitorServer.ListenAndServe())
}

func initTemplate() {
	const html = `
	<!DOCTYPE html><html><head><meta charset="UTF-8"><title>Proxy Monitor</title><meta http-equiv="refresh" content="2">
	<style>
		body { font-family: 'Segoe UI', Arial, sans-serif; background: #f0f2f5; margin: 0; padding: 20px; }
		.container { max-width: 1000px; margin: auto; }
		.sys-header { background: #1a73e8; color: white; padding: 15px; border-radius: 8px; margin-bottom: 20px; font-weight: 500; }
		.card { background: white; border-radius: 8px; margin-bottom: 20px; box-shadow: 0 2px 8px rgba(0,0,0,0.1); overflow: hidden; }
		.header-row { display: flex; justify-content: space-between; align-items: center; padding: 12px 15px; background: #fff; border-bottom: 1px solid #eee; }
		.progress-bar { background: #e9ecef; height: 14px; border-radius: 7px; overflow: hidden; margin-top: 8px; }
		.progress-fill { background: linear-gradient(90deg, #1a73e8, #34a853); height: 100%; transition: width 0.5s ease; }
		table { width: 100%; border-collapse: collapse; }
		th { background: #f8f9fa; color: #666; font-size: 0.8em; text-transform: uppercase; padding: 10px; text-align: left; }
		td { padding: 10px; border-bottom: 1px solid #f1f3f4; font-size: 0.95em; }
		.up { color: #28a745; font-weight: bold; } .down { color: #dc3545; font-weight: bold; }
		.badge { background: #e8f0fe; color: #1a73e8; padding: 2px 8px; border-radius: 4px; font-size: 0.8em; }
	</style></head>
	<body><div class="container">
		<div class="sys-header" style="padding:15px; display:flex; justify-content:space-between;">
			<span>📈 App Usage: <b>{{printf "%.1f" .AppCPU}}%</b> Load | <b>{{printf "%.1f" .AppRAM}}</b> MB RAM</span>
			<span>📈 System: <b>{{printf "%.1f" .SysCPU}}%</b> Load | <b>{{printf "%.1f" .SysRAM}}%</b> RAM</span>
		</div>
		<div class="card" style="padding: 15px;">
			<div style="display:flex; justify-content:space-between; font-weight:bold;">
				<span>Tổng kết nối toàn hệ thống</span><span>{{.TotalActive}} / {{.MaxGlobal}}</span>
			</div>
			<div class="progress-bar"><div class="progress-fill" style="width: {{multi .TotalActive .MaxGlobal}}%"></div></div>
		</div>
		<h2>🚀 Active Services</h2>
		{{range .Services}}{{if not .ErrorMsg}}
		<div class="card">
			<div class="header-row">
				<span style="font-weight:bold;">{{.Name}} (Port: {{.Port}}) {{if .BypassLimits}}<small style="color:orange;">[BYPASS]</small>{{end}}</span>
				<span class="badge">{{.Protocol}}</span>
			</div>
			<table><thead><tr><th width="50%">Backend</th><th width="30%">Status</th><th width="20%">Active</th></tr></thead>
				<tbody>{{range .Backends}}<tr><td><code>{{.Address}}</code></td>
				<td>{{if .Alive}} <span class="up">● ONLINE</span> {{else}} <span class="down">○ OFFLINE</span> {{end}}</td>
				<td><strong>{{.ActiveConns}}</strong></td></tr>{{end}}</tbody>
			</table>
		</div>
		{{end}}{{end}}
	</div></body></html>`

	mainTmpl = template.Must(template.New("m").Funcs(template.FuncMap{
		"multi": func(curr, max int64) float64 {
			if max <= 0 { return 0 }
			val := float64(curr) / float64(max) * 100
			if val > 100 { return 100 }
			return val
		},
		"add": func(a, b int) int { return a + b },
	}).Parse(html))
}
