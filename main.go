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
	mux          sync.RWMutex
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
	penaltyBox             sync.Map
	rateMap                sync.Map
	mainTmpl               *template.Template
	totalActiveConns       int64

	appCPUUsage uint64
	sysCPUUsage uint64
	sysRAMUsage uint64
)

var bufPool = sync.Pool{
	New: func() interface{} { return make([]byte, 32*1024) },
}

// --- LOGGERS ---
func infoLog(format string, v ...interface{}) { logWithLevel("INFO", format, v...) }
func warnLog(format string, v ...interface{}) { logWithLevel("WARN", format, v...) }
func errLog(format string, v ...interface{})  { logWithLevel("ERROR", format, v...) }
func logWithLevel(level, format string, v ...interface{}) {
	t := time.Now().Format("15:04:05 02/01/2006")
	fmt.Printf("[%s] [%s] "+format+"\n", append([]interface{}{t, level}, v...)...)
}

func main() {
	fmt.Println("===========================================")
	fmt.Println("   TCP PROXY - PRODUCTION ULTIMATE 2025")
	fmt.Println("===========================================")

	initTemplate()
	reloadConfig(true)
	go watchConfigFile()
	
	startMonitorServer(CurrentMonitorPort)
	select {}
}

func initTemplate() {
	const html = `
	<!DOCTYPE html><html><head><meta charset="UTF-8"><title>Proxy Monitor</title><meta http-equiv="refresh" content="2">
	<style>
		body { font-family: 'Segoe UI', Arial, sans-serif; background: #f0f2f5; margin: 0; padding: 20px; }
		.container { max-width: 1000px; margin: auto; }
		.sys-header { background: #1a73e8; color: white; padding: 15px; border-radius: 8px; margin-bottom: 20px; font-weight: 500; box-shadow: 0 4px 6px rgba(0,0,0,0.1); }
		.card { background: white; border-radius: 8px; margin-bottom: 20px; box-shadow: 0 2px 8px rgba(0,0,0,0.1); overflow: hidden; }
		.header-row { display: flex; justify-content: space-between; align-items: center; padding: 12px 15px; background: #fff; border-bottom: 1px solid #eee; }
		.progress-container { padding: 15px; border-bottom: 1px solid #eee; }
		.progress-bar { background: #e9ecef; height: 14px; border-radius: 7px; overflow: hidden; margin-top: 8px; }
		.progress-fill { background: linear-gradient(90deg, #1a73e8, #34a853); height: 100%; transition: width 0.5s ease; }
		table { width: 100%; border-collapse: collapse; table-layout: fixed; }
		th { background: #f8f9fa; color: #666; font-size: 0.8em; text-transform: uppercase; padding: 10px 15px; text-align: left; }
		td { padding: 12px 15px; border-bottom: 1px solid #f1f3f4; font-size: 0.95em; vertical-align: middle; }
		.status-cell { width: 130px; }
		.status-box { display: inline-flex; align-items: center; width: 110px; font-family: 'Consolas', 'Monaco', monospace; font-weight: bold; font-size: 0.9em; }
		.dot { margin-right: 8px; font-size: 1.2em; }
		.up { color: #28a745; } .down { color: #dc3545; }
		.badge { background: #e8f0fe; color: #1a73e8; padding: 2px 8px; border-radius: 4px; font-size: 0.8em; border: 1px solid #d2e3fc; }
		.error-list { background: #fff; border-radius: 8px; padding: 15px; box-shadow: 0 2px 8px rgba(0,0,0,0.1); }
		.err-msg { color: #cf1322; background: #fff1f0; border: 1px solid #ffa39e; padding: 10px; border-radius: 5px; margin-bottom: 5px; }
	</style></head>
	<body><div class="container">
		<div class="sys-header" style="padding:15px; display:flex; justify-content:space-between;">
			<span>📈 App Usage: <b>{{printf "%.1f" .AppCPU}}%</b> Load | <b>{{printf "%.1f" .AppRAM}}</b> MB RAM</span>
			<span>📈 System: <b>{{printf "%.1f" .SysCPU}}%</b> Load | <b>{{printf "%.1f" .SysRAM}}%</b> RAM</span>
		</div>
		<div class="card progress-container">
			<div style="display:flex; justify-content:space-between; font-weight:bold;">
				<span>Tổng kết nối toàn hệ thống</span><span>{{.TotalActive}} / {{.MaxGlobal}}</span>
			</div>
			<div class="progress-bar"><div class="progress-fill" style="width: {{multi .TotalActive .MaxGlobal}}%"></div></div>
		</div>
		<h2>🚀 Active Services</h2>
		{{range .Services}}{{if not .ErrorMsg}}
		<div class="card">
			<div class="header-row"><span style="font-weight:bold;">{{.Name}} (Port: {{.Port}})</span><span class="badge">{{.Protocol}}</span></div>
			<table><thead><tr><th width="50%">Backend</th><th class="status-cell">Status</th><th width="20%">Active</th></tr></thead>
				<tbody>{{range .Backends}}<tr><td><code>{{.Address}}</code></td>
				<td class="status-cell">{{if .Alive}} <span class="status-box up"><span class="dot">●</span>ONLINE</span>
					{{else}} <span class="status-box down"><span class="dot">○</span>OFFLINE</span> {{end}}</td>
				<td><strong>{{.ActiveConns}}</strong></td></tr>{{end}}</tbody>
			</table>
		</div>
		{{end}}{{end}}
		<h2>⚠️ Warnings</h2>
		<div class="error-list">
			{{$totalErrors := 0}}{{range .Services}}{{if .ErrorMsg}}{{$totalErrors = add $totalErrors 1}}<div class="err-msg"><strong>Port {{.Port}}</strong>: {{.ErrorMsg}}</div>{{end}}
				{{range .Backends}}{{if not .Alive}}{{$totalErrors = add $totalErrors 1}}<div class="err-msg"><strong>Backend Offline</strong>: {{.Address}}</div>{{end}}{{end}}{{end}}
			{{if eq $totalErrors 0}}<p style="color: #52c41a; font-weight: bold; margin:0;">● All systems operational.</p>{{end}}
		</div>
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
			newPorts[port] = true
			if svc, exists := services[port]; exists {
				svc.mux.Lock()
				svc.Name, svc.Protocol = name, proto
				oMap := make(map[string]*Backend)
				for _, b := range svc.Backends { oMap[b.Address] = b }
				var updated []*Backend
				for _, a := range strings.Split(bAddr, ",") {
					if oldB, ok := oMap[a]; ok { updated = append(updated, oldB) } else { updated = append(updated, &Backend{Address: a, Alive: false}) }
				}
				svc.Backends = updated
				if svc.ErrorMsg != "" { go svc.startProxy() }
				svc.mux.Unlock()
			} else {
				newSvc := &ProxyService{Name: name, Port: port, Protocol: proto, quit: make(chan bool)}
				for _, a := range strings.Split(bAddr, ",") { newSvc.Backends = append(newSvc.Backends, &Backend{Address: a, Alive: false}) }
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
	infoLog("Proxy [%s] running on port %s (%s)", s.Name, s.Port, s.Protocol)

	for {
		conn, err := ln.Accept()
		if err != nil { return }
		ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())

		// 1. PENALTY BOX LOG
		if ban, blocked := penaltyBox.Load(ip); blocked {
			if time.Now().Before(ban.(time.Time)) {
				warnLog("Block IP %s: Still in Penalty Box", ip) // Khôi phục log
				conn.Close(); continue
			}
			penaltyBox.Delete(ip)
		}

		// 2. RATE LIMIT LOG
		v, _ := rateMap.LoadOrStore(ip, &IPStat{lastReset: time.Now()})
		stat := v.(*IPStat)
		window := time.Duration(atomic.LoadInt64(&connRateWindowNS))
		if time.Since(stat.lastReset) > window {
			atomic.StoreInt32(&stat.count, 1); stat.lastReset = time.Now()
		} else {
			if atomic.AddInt32(&stat.count, 1) > atomic.LoadInt32(&connRateLimit) {
				p := time.Duration(atomic.LoadInt64(&penaltyDurationNS))
				warnLog("Block IP %s: Rate limit exceeded (Penalty: %v)", ip, p) // Khôi phục log
				penaltyBox.Store(ip, time.Now().Add(p))
				atomic.StoreInt32(&stat.count, 0)
				stat.lastReset = time.Now()
				conn.Close(); continue
			}
		}

		// 3. MAX CONNS PER IP LOG
		valIP, _ := s.IPMap.LoadOrStore(ip, new(int32))
		activeIPCounter := valIP.(*int32)
		if atomic.AddInt32(activeIPCounter, 1) > atomic.LoadInt32(&MaxConnsPerIP) {
			warnLog("Block IP %s: Reached MaxConnsPerIP (%d)", ip, atomic.LoadInt32(&MaxConnsPerIP)) // Khôi phục log
			atomic.AddInt32(activeIPCounter, -1)
			conn.Close(); continue
		}

		// 4. GLOBAL LIMIT LOG
		if atomic.LoadInt64(&totalActiveConns) >= atomic.LoadInt64(&MaxGlobalConns) {
			warnLog("Block connection: Global limit reached (%d)", atomic.LoadInt64(&MaxGlobalConns)) // Khôi phục log
			atomic.AddInt32(activeIPCounter, -1)
			conn.Close(); continue
		}

		atomic.AddInt64(&totalActiveConns, 1)
		atomic.AddInt64(&s.CurrentConns, 1)
		go s.handleConnection(conn, activeIPCounter)
	}
}

// ... (Các hàm khác: checkBackendsOnce, startHealthCheck, handleConnection, stop, getNextAliveBackend, watchConfigFile, startMonitorServer GIỮ NGUYÊN) ...

func (s *ProxyService) checkBackendsOnce() {
	s.mux.RLock(); backends := s.Backends; s.mux.RUnlock()
	for _, b := range backends {
		conn, err := net.DialTimeout("tcp", b.Address, 1*time.Second)
		b.mux.Lock(); b.Alive = (err == nil); if err == nil { conn.Close() }; b.mux.Unlock()
	}
}
func (s *ProxyService) startHealthCheck() {
	for {
		interval := time.Duration(atomic.LoadInt64(&healthCheckIntervalNS))
		if interval < 1*time.Second { interval = 3 * time.Second }; time.Sleep(interval)
		select { case <-s.quit: return; default: s.checkBackendsOnce() }
	}
}
func (s *ProxyService) handleConnection(clientConn net.Conn, ipCounter *int32) {
	defer func() {
		if r := recover(); r != nil { errLog("Panic: %v", r) }; clientConn.Close()
		atomic.AddInt64(&totalActiveConns, -1); atomic.AddInt64(&s.CurrentConns, -1)
		if ipCounter != nil { atomic.AddInt32(ipCounter, -1) }
	}()
	s.mux.RLock(); proto, backend := s.Protocol, s.getNextAliveBackend(); s.mux.RUnlock()
	if backend == nil { return }; targetConn, err := net.DialTimeout("tcp", backend.Address, 5*time.Second)
	if err != nil { return }; defer targetConn.Close()
	atomic.AddInt64(&backend.ActiveConns, 1); defer atomic.AddInt64(&backend.ActiveConns, -1)
	var timeout time.Duration
	if proto == "http" { timeout = time.Duration(atomic.LoadInt64(&idleTimeoutHTTPNS)) } else { timeout = time.Duration(atomic.LoadInt64(&idleTimeoutTCPNS)) }
	clientConn.SetDeadline(time.Now().Add(timeout)); targetConn.SetDeadline(time.Now().Add(timeout))
	done := make(chan bool, 2); cp := func(dst io.Writer, src io.Reader) {
		buf := bufPool.Get().([]byte); defer bufPool.Put(buf); io.CopyBuffer(dst, src, buf); done <- true
	}
	go cp(targetConn, &IdleTimeoutConn{clientConn, timeout}); go cp(clientConn, &IdleTimeoutConn{targetConn, timeout}); <-done
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
