package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"gopkg.in/yaml.v3"
)

type runtimeConfig struct {
	ruleAP net.IP
	config
	tls          *tls.Certificate
	records      map[string]net.IP
	certificates map[string]*tls.Certificate
}

type application struct {
	keylogs                                                *tlsKeylog
	dir, lanDevice, apSection, dnsUpstream, modemInterface string
	lanAddr                                                netip.Addr
	lanPrefix                                              netip.Prefix
	lanPrefixes                                            atomic.Value
	sitePort, adminPort, resolverPort, dnsPort             int
	adminPassword, csrf                                    string
	stationSupported                                       bool
	firewallEnabled, guardEnabled                          bool
	monitor                                                *monitor
	requestLogs                                            *requestLog
	eventUpload                                            *eventUploader
	loggingMu                                              sync.RWMutex
	uploadCancel                                           context.CancelFunc
	current                                                atomic.Pointer[runtimeConfig]
	update                                                 sync.Mutex
}

func randomToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

func (app *application) prepare(cfg config) (*runtimeConfig, error) {
	// Monitor mode has no business domain: reuse the existing local certificate
	// without replacing it with a certificate for the monitor placeholder.
	if cfg.MonitorOnly {
		pair, err := tls.LoadX509KeyPair(filepath.Join(app.dir, "local.crt"), filepath.Join(app.dir, "local.key"))
		if err == nil {
			return app.runtimeSettings(cfg, &pair, map[string]*tls.Certificate{}), nil
		}
		for _, name := range []string{"local.crt", "local.key"} {
			if _, e := os.Lstat(filepath.Join(app.dir, name)); !os.IsNotExist(e) {
				return nil, err
			}
		}
		generated, err := certificate(app.dir, cfg.primarySite(), app.lanAddr.AsSlice())
		if err != nil {
			return nil, err
		}
		return app.runtimeSettings(cfg, generated, map[string]*tls.Certificate{}), nil
	}
	if _, err := certificate(app.dir, siteConfig{Domain: cfg.primarySite().Domain}, app.lanAddr.AsSlice()); err != nil {
		return nil, err
	}
	certificates := make(map[string]*tls.Certificate)
	for i, site := range cfg.sites() {
		dir := app.dir
		if i > 0 && site.Certificate == "" {
			dir = filepath.Join(app.dir, "sites", site.Domain)
			if err := os.MkdirAll(dir, 0700); err != nil {
				return nil, err
			}
		}
		pair, err := certificate(dir, site, app.lanAddr.AsSlice())
		if err != nil {
			return nil, err
		}
		certificates[site.Domain] = pair
	}
	return app.runtimeSettings(cfg, certificates[cfg.primarySite().Domain], certificates), nil
}

func (app *application) runtimeSettings(cfg config, pair *tls.Certificate, certs ...map[string]*tls.Certificate) *runtimeConfig {
	settings := &runtimeConfig{ruleAP: app.lanAddr.AsSlice(), config: cfg, tls: pair, records: make(map[string]net.IP), certificates: map[string]*tls.Certificate{cfg.primarySite().Domain: pair}}
	if len(certs) > 0 {
		settings.certificates = certs[0]
	}
	if cfg.MonitorOnly {
		return settings
	}
	for _, site := range cfg.sites() {
		settings.records[site.Domain] = app.lanAddr.AsSlice()

	}
	for _, record := range cfg.DNS {
		address := app.lanAddr.AsSlice()
		if record.IP != "ap" {
			address = net.ParseIP(record.IP).To4()
		}
		settings.records[record.Name] = address
	}
	return settings
}

func (app *application) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	current := app.current.Load()
	if current.MonitorOnly {
		return nil, fmt.Errorf("业务 HTTPS 在仅监控模式下禁用")
	}
	if hello.ServerName == "" {
		return current.tls, nil
	}
	if pair := current.certificates[current.config.siteKey(hello.ServerName)]; pair != nil {
		return pair, nil
	}
	return nil, fmt.Errorf("unconfigured HTTPS domain: %s", hello.ServerName)
}

func command(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

func uciGet(key string) string {
	data, _ := command(2*time.Second, "uci", "-q", "get", key)
	return strings.TrimSuffix(string(data), "\n")
}

func (app *application) replaceConfig(data []byte) error {
	app.update.Lock()
	defer app.update.Unlock()
	return app.replaceConfigLocked(data)
}

// Caller holds app.update, including filename-checked YAML updates.
func (app *application) replaceConfigLocked(data []byte) error {
	cfg, err := app.validateImport(data)
	if err != nil {
		return err
	}
	settings, err := app.prepare(cfg)
	if err != nil {
		return err
	}
	if err := app.commitSettings(settings, nil); err != nil {
		return err
	}
	app.monitor.message("已加载 YAML 配置")
	return nil
}

func escapedDomain(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

func responseJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

// Network state is observational; OpenWrt owns all network configuration.
func actualUpstream(ipv4, ipv6 []byte, modemDevice string) string {
	if modemDevice != "" {
		for _, route := range []string{string(ipv4), string(ipv6)} {
			fields := strings.Fields(route)
			for i := 0; i+1 < len(fields); i++ {
				if fields[i] == "dev" && fields[i+1] == modemDevice {
					return "sim"
				}
			}
		}
	}
	return "openwrt"
}

func (app *application) status() map[string]any {
	route, _ := command(1500*time.Millisecond, "ip", "-4", "route", "show", "default")
	ipv6, _ := command(1500*time.Millisecond, "ip", "-6", "route", "show", "default")
	device, _ := command(1500*time.Millisecond, "ubus", "call", "network.interface."+app.modemInterface, "status")
	modem := struct {
		Up      bool   `json:"up"`
		Pending bool   `json:"pending"`
		Device  string `json:"l3_device"`
	}{}
	_ = json.Unmarshal(device, &modem)
	return map[string]any{
		"event_upload": app.uploadStatus(), "ok": true, "uptime": time.Since(started).Truncate(time.Second).String(),
		"upstream": actualUpstream(route, ipv6, modem.Device), "wan_ipv4": len(route) > 0, "wan_ipv6": len(ipv6) > 0,
		"modem_registered": modem.Up, "modem_device": modem.Device, "modem_pending": modem.Pending,
		"ap_ssid": uciGet("wireless." + app.apSection + ".ssid"), "ap_ip": app.lanAddr.String(), "station_supported": app.stationSupported,
	}
}

func (app *application) publicSite(writer http.ResponseWriter, req *http.Request) {
	current := app.current.Load()
	if current.MonitorOnly || !app.scopedClient(current.Scope, req.RemoteAddr) {
		http.NotFound(writer, req)
		return
	}
	siteKey := current.siteKey(escapedDomain(req.Host))
	if siteKey == "" {
		http.NotFound(writer, req)
		return
	}
	var routes []customRoute
	for _, site := range current.Sites {
		if site.Domain == siteKey {
			routes = site.Routes
			break
		}
	}
	matched, found := matchingRoute(routes, req.URL.Path)
	if found {
		req, finishBodyLog := app.traceRequestBody(req)
		app.monitor.message(routeHitMessage(req, matched.Path, "开始读取请求体"))
		app.recordRequest(req)
		finishBodyLog()
		if matched.ResponseMode == "close" {
			action := "关闭连接"
			if req.ProtoMajor == 2 {
				action = "重置 HTTP/2 当前流"
			}
			app.monitor.message(routeHitMessage(req, matched.Path, action))
			closeRequestConnection(writer, req)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Checksum", fmt.Sprintf("%x", md5.Sum([]byte(matched.JSON))))
		n, err := io.WriteString(writer, matched.JSON)
		action := fmt.Sprintf("返回 JSON（HTTP 200，已写入 %d 字节）", n)
		if err != nil {
			action = fmt.Sprintf("JSON 写入失败（已写入 %d 字节，错误=%q）", n, err.Error())
		}
		app.monitor.message(routeHitMessage(req, matched.Path, action))
		return
	}
	http.NotFound(writer, req)
}

func (app *application) admin(writer http.ResponseWriter, req *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("X-Frame-Options", "DENY")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self' wss:")
	user, password, ok := req.BasicAuth()
	if !ok || user != "admin" || subtle.ConstantTimeCompare([]byte(password), []byte(app.adminPassword)) != 1 {
		writer.Header().Set("WWW-Authenticate", `Basic realm="Wi-Fi administrator", charset="UTF-8"`)
		http.Error(writer, "Authentication required", 401)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead &&
		subtle.ConstantTimeCompare([]byte(req.Header.Get("X-Repeater-Token")), []byte(app.csrf)) != 1 {
		http.Error(writer, "Invalid request token", 403)
		return
	}
	app.adminRoutes(writer, req, false)
}

func (app *application) adminRoutes(writer http.ResponseWriter, req *http.Request, local bool) {
	switch req.URL.Path {
	case "/api/fsd/logging":
		if !local {
			http.Error(writer, "LuCI authentication required", 403)
			return
		}
		app.loggingAPI(writer, req)
	case "/api/status":
		responseJSON(writer, 200, app.status())
	case "/api/scan":
		interfaces, err := command(3*time.Second, "ubus", "call", "iwinfo", "devices")
		decoded := struct {
			Devices []string `json:"devices"`
		}{}
		if err != nil || json.Unmarshal(interfaces, &decoded) != nil || len(decoded.Devices) == 0 {
			http.Error(writer, "无线接口不可用", 503)
			return
		}
		params, _ := json.Marshal(map[string]string{"device": decoded.Devices[0]})
		data, err := command(12*time.Second, "ubus", "call", "iwinfo", "scan", string(params))
		if err != nil || !json.Valid(data) {
			http.Error(writer, "无线扫描失败", 503)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write(data)
	case "/api/config":
		if req.Method == http.MethodGet {
			cfg := app.current.Load().publicConfig()
			cfg = cfg.publicConfig()
			content, _ := yaml.Marshal(cfg)
			result := map[string]string{"yaml": string(content)}
			if !local {
				result["token"] = app.csrf
			}
			responseJSON(writer, 200, result)
			return
		}
		if req.Method != http.MethodPut {
			http.Error(writer, "use PUT to import YAML", 405)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(writer, req.Body, 128*1024))
		if err == nil {
			err = app.replaceConfig(body)
		}
		if err != nil {
			http.Error(writer, err.Error(), 400)
			return
		}
		responseJSON(writer, 200, map[string]bool{"ok": true})
	case "/api/export":
		cfg := app.current.Load().publicConfig()
		content, _ := yaml.Marshal(cfg.publicConfig())
		writer.Header().Set("Content-Type", "application/yaml")
		writer.Header().Set("Content-Disposition", `attachment; filename="fsd-service.yaml"`)
		_, _ = writer.Write(content)
	case "/api/dns":
		responseJSON(writer, 200, app.current.Load().DNSRules)
	case "/api/h2site":
		cfg := app.current.Load().publicConfig()
		responseJSON(writer, 200, cfg.Sites)
	case "/api/h2routes":
		responseJSON(writer, 200, app.current.Load().Sites)
	case "/api/log":
		app.monitor.Lock()
		logs := slices.Clone(app.monitor.logs)
		app.monitor.Unlock()
		if local {
			slices.Reverse(logs)
			logs = boundSnapshotRows(logs, 4000)
			slices.Reverse(logs)
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(writer, strings.Join(logs, "\n"))
	case "/api/fsd/probe":
		if !local {
			http.NotFound(writer, req)
			return
		}
		responseJSON(writer, 200, map[string]any{"results": app.protocolProbe(req.Context(), app.current.Load())})
	case "/api/fsd/checks":
		if !local || req.Method != http.MethodGet {
			http.NotFound(writer, req)
			return
		}
		results, err := app.runDiagnostics(req.Context(), req.URL.Query().Get("group"))
		if err != nil {
			http.Error(writer, err.Error(), 400)
			return
		}
		responseJSON(writer, 200, map[string]any{"results": results})
	case "/api/fsd/validate":
		if !local || req.Method != http.MethodPost {
			http.NotFound(writer, req)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(writer, req.Body, 128*1024))
		if err == nil {
			_, err = app.validateImport(body)
		}
		if err != nil {
			http.Error(writer, err.Error(), 400)
			return
		}
		responseJSON(writer, 200, map[string]bool{"ok": true})
	case "/api/fsd/profiles":
		if !local {
			http.NotFound(writer, req)
			return
		}
		app.profilesAPI(writer, req)
	case "/api/fsd/config-editor":
		if !local {
			http.NotFound(writer, req)
			return
		}
		app.configEditor(writer, req)
	case "/api/mon/live":
		app.monitor.Lock()
		clients := make([]clientDevice, 0, len(app.monitor.clients))
		connections := make([]connection, 0, len(app.monitor.connections))
		for _, item := range app.monitor.clients {
			clients = append(clients, item)
		}
		for _, item := range app.monitor.connections {
			connections = append(connections, item)
		}
		enabled, capture := app.monitor.enabled, app.monitor.packetCapture
		temporary := slices.Clone(app.monitor.blocked)
		app.monitor.Unlock()
		slices.SortFunc(clients, func(a, b clientDevice) int { return strings.Compare(a.IP, b.IP) })
		slices.SortFunc(connections, func(a, b connection) int {
			if order := b.lastActive.Compare(a.lastActive); order != 0 {
				return order
			}
			return strings.Compare(
				flowKey(a.Protocol, a.Client, a.SourcePort, a.Remote, a.Port),
				flowKey(b.Protocol, b.Client, b.SourcePort, b.Remote, b.Port),
			)
		})
		totalConnections := len(connections)
		if local {
			clients = boundSnapshotRows(clients, 6500)
			connections = boundSnapshotRows(connections, 10000)
			temporary = boundSnapshotRows(temporary, 3000)
		}
		status := map[string]any{"devices": clients, "connections": connections, "enabled": enabled,
			"packet_capture": capture, "temporary_blocks": temporary, "connection_count": totalConnections, "event_upload": app.uploadStatus()}
		responseJSON(writer, 200, status)
	case "/api/mon/dns":
		since, _ := strconv.ParseUint(req.URL.Query().Get("since"), 10, 64)
		app.monitor.Lock()
		events := make([]dnsEvent, 0, len(app.monitor.events))
		for _, event := range app.monitor.events {
			if event.ID > since {
				events = append(events, event)
			}
		}
		app.monitor.Unlock()
		if local {
			slices.Reverse(events)
			events = boundSnapshotRows(events, 6500)
			slices.Reverse(events)
		}
		responseJSON(writer, 200, events)
	case "/api/mon/block":
		if req.Method == http.MethodGet {
			responseJSON(writer, 200, app.monitor.temporaryRules())
			return
		}
		name := domainName(req.FormValue("domain"))
		action := req.FormValue("action")
		if req.Method != http.MethodPost || !validDomain(name, false) || (action != "add" && action != "del") {
			http.Error(writer, "invalid domain or action", 400)
			return
		}
		if err := app.monitor.setTemporaryRule(name, action == "add"); err != nil {
			http.Error(writer, err.Error(), 400)
			return
		}
		app.monitor.message("临时域名规则 " + action + " " + name)
		responseJSON(writer, 200, map[string]bool{"ok": true})
	case "/api/mon/ctl":
		if req.Method != http.MethodPost {
			http.Error(writer, "use POST", 405)
			return
		}
		app.monitor.Lock()
		switch req.FormValue("action") {
		case "enable":
			app.monitor.enabled = true
		case "disable":
			app.monitor.enabled = false
		case "clear_logs":
			app.monitor.logs = nil
		case "reset_perf":
			app.monitor.events = nil
			app.monitor.clients = make(map[string]clientDevice)
			app.monitor.connections = make(map[string]connection)
		default:
			app.monitor.Unlock()
			http.Error(writer, "invalid action", 400)
			return
		}
		app.monitor.Unlock()
		responseJSON(writer, 200, map[string]bool{"ok": true})
	case "/api/mon/bench":
		start := time.Now()
		connections, err := connectionsFromKernel()
		if err != nil {
			http.Error(writer, "连接统计不可用", 503)
			return
		}
		responseJSON(writer, 200, map[string]any{"entries": len(connections), "sample_duration_us": time.Since(start).Microseconds()})
	case "/api/restart":
		if req.Method != http.MethodPost {
			http.Error(writer, "use POST", 405)
			return
		}
		responseJSON(writer, 200, map[string]bool{"ok": true})
		go func() {
			time.Sleep(2 * time.Second)
			_, _ = command(5*time.Second, "reboot")
		}()
	default:
		http.NotFound(writer, req)
	}
}

// Allow room for rpcd to convert the combined LuCI monitor response to blobmsg.
func boundSnapshotRows[T any](rows []T, budget int) []T {
	for i, row := range rows {
		encoded, err := json.Marshal(row)
		budget -= len(encoded) + 1
		if err != nil || budget < 0 {
			return rows[:i]
		}
	}
	return rows
}

var started = time.Now()

func main() {
	log.SetFlags(0)
	log.SetOutput(systemLogWriter{output: os.Stderr})
	if len(os.Args) > 1 && os.Args[1] == "-rpcd" {
		runFSDRPC(os.Args[2:])
		return
	}
	var app application
	var ip, listen string
	var check, noFilter, luci, forwardGuard, printGuard bool
	flag.StringVar(&app.dir, "config-dir", "/etc/fsd", "private configuration directory")
	flag.StringVar(&ip, "lan", "", "LAN IPv4 address and prefix (required)")
	flag.StringVar(&app.lanDevice, "lan-device", "br-lan", "LAN interface")
	flag.StringVar(&app.modemInterface, "modem-interface", "wan", "UCI cellular interface section")
	flag.StringVar(&app.apSection, "ap-section", "ap0", "UCI wireless access point section")
	flag.StringVar(&listen, "listen", "", "HTTPS bind address (defaults to LAN IP)")
	flag.IntVar(&app.sitePort, "site-port", 443, "HTTPS site port")
	flag.IntVar(&app.adminPort, "admin-port", 8444, "HTTPS admin port")
	flag.IntVar(&app.dnsPort, "dns-port", 53, "DNS listening port")
	flag.IntVar(&app.resolverPort, "resolver-port", 5355, "original dnsmasq port")
	flag.BoolVar(&app.stationSupported, "sta-supported", false, "wireless driver supports concurrent AP and STA")
	flag.BoolVar(&luci, "luci", false, "use the LuCI RPC socket instead of the independent admin listener")
	flag.BoolVar(&check, "check", false, "validate config and certificate only")
	flag.BoolVar(&noFilter, "no-filter", false, "skip Linux firewall integration for local tests")
	flag.BoolVar(&forwardGuard, "forward-guard", false, "enable the fw4 forwarding lease after FSD is ready")
	flag.BoolVar(&printGuard, "print-guard", false, "print boot firewall rules for configured device scope")
	flag.Parse()
	app.firewallEnabled, app.guardEnabled = !noFilter, forwardGuard
	if forwardGuard && noFilter {
		log.Fatal("forward-guard requires firewall integration")
	}
	var err error
	app.lanPrefix, err = netip.ParsePrefix(ip)
	if err != nil || !app.lanPrefix.Addr().Is4() ||
		!regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`).MatchString(app.lanDevice) ||
		!regexp.MustCompile(`^[a-zA-Z0-9_]{1,64}$`).MatchString(app.modemInterface) ||
		!regexp.MustCompile(`^[a-zA-Z0-9_]{1,64}$`).MatchString(app.apSection) {
		log.Fatal("invalid LAN address or interface")
	}
	for _, port := range []int{app.sitePort, app.adminPort, app.dnsPort, app.resolverPort} {
		if port < 1 || port > 65535 {
			log.Fatal("invalid port")
		}
	}
	app.lanAddr = app.lanPrefix.Addr()
	app.dnsUpstream = net.JoinHostPort("127.0.0.1", strconv.Itoa(app.resolverPort))
	if listen == "" {
		listen = app.lanAddr.String()
	}
	app.monitor = newMonitor()
	app.refreshLAN()
	if !check && !printGuard {
		if err := app.initProfiles(); err != nil {
			log.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(app.dir, "config.yaml"))
	if err != nil {
		log.Fatal(err)
	}
	cfg, err := decodeConfig(data)
	if err != nil {
		log.Fatal(err)
	}
	if printGuard {
		_, _ = os.Stdout.Write(app.guardFile(cfg.Scope, cfg.MonitorOnly))
		return
	}
	settings, err := app.prepare(cfg)
	if err != nil {
		log.Fatal(err)
	}
	app.current.Store(settings)
	app.eventUpload, err = loadEventUploader(app.dir)
	if err != nil {
		log.Fatal(err)
	}
	if check {
		if !noFilter {
			if err := app.installFilter(true); err != nil {
				log.Fatal(err)
			}
		}
		return
	}
	if app.eventUpload != nil {
		ctx, cancel := context.WithCancel(context.Background())
		app.uploadCancel = cancel
		go app.eventUpload.run(ctx)
	}
	loggingConfig, err := readLoggingSettings(app.dir)
	if err != nil {
		log.Fatal(err)
	}
	app.keylogs = newTLSKeylog("/tmp/fsd/tls-keylogs")
	app.keylogs.setEnabled(loggingConfig.TLSKeylog)
	go app.keylogs.run(context.Background())
	if *loggingConfig.LocalCSV {
		app.requestLogs = &requestLog{dir: filepath.Join(app.dir, "request-logs"), limit: 100 * 1024 * 1024}
	}
	if luci {
		if err := app.startLocalAdministration(); err != nil {
			log.Fatal(err)
		}
	} else {
		keyFile := filepath.Join(app.dir, "admin-password")
		if data, err = os.ReadFile(keyFile); errors.Is(err, os.ErrNotExist) {
			data = []byte(randomToken() + "\n")
			err = writePrivate(keyFile, data)
		}
		if err != nil || len(strings.TrimSpace(string(data))) < 24 {
			log.Fatal("admin password file is unavailable or invalid")
		}
		app.adminPassword, app.csrf = strings.TrimSpace(string(data)), randomToken()
	}
	if !noFilter {
		if err := app.installFilter(false); err != nil {
			log.Fatal(err)
		}
		go app.syncBlocklist()
		go app.sampleConnections()
		go func() {
			for {
				if err := app.captureNames(); err != nil {
					log.Printf("LAN metadata capture unavailable: %v", err)
				}
				time.Sleep(20 * time.Second)
			}
		}()
	}
	dnsStarted := make(chan struct{}, 2)
	for _, network := range []string{"udp", "tcp"} {
		server := &dns.Server{Addr: ":" + strconv.Itoa(app.dnsPort), Net: network, Handler: dns.HandlerFunc(app.serveDNS),
			UDPSize: 4096, ReadTimeout: 4 * time.Second, WriteTimeout: 4 * time.Second,
			NotifyStartedFunc: func() { dnsStarted <- struct{}{} }}
		go func() {
			log.Fatal(server.ListenAndServe())
		}()
	}
	type httpsEndpoint struct {
		port    int
		handler http.HandlerFunc
	}
	endpoints := []httpsEndpoint{{app.sitePort, app.publicSite}}
	if !luci {
		endpoints = append(endpoints, httpsEndpoint{app.adminPort, app.admin})
	}
	for _, endpoint := range endpoints {
		tracker := &tlsEventLog{names: make(map[string]string), app: &app, output: log.Writer()}
		server := &http.Server{
			Addr: net.JoinHostPort(listen, strconv.Itoa(endpoint.port)), Handler: endpoint.handler,
			ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 25 * time.Second, IdleTimeout: 60 * time.Second,
			MaxHeaderBytes: 32768,
			ConnContext:    requestConnectionContext,
			TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12,
				GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
					if endpoint.port == app.adminPort && app.current.Load().MonitorOnly {
						return app.current.Load().tls, nil
					}
					if endpoint.port == app.sitePort {
						tracker.hello(hello.Conn, hello.ServerName)
					}
					return app.getCertificate(hello)
				}},
		}
		if endpoint.port == app.sitePort {
			server.TLSConfig.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				return keylogTLSConfig(server.TLSConfig, app.current.Load(), hello.Conn.RemoteAddr().String(), app.keylogs), nil
			}
			server.ConnContext = tracker.connection
			server.ConnState = tracker.state
			server.ErrorLog = log.New(tracker, "", 0)
		}
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			log.Fatal(server.ServeTLS(listener, "", ""))
		}()
	}
	for range 2 {
		<-dnsStarted
	}
	app.monitor.message("热点服务已启动")
	if forwardGuard {
		app.maintainForwardGuard()
		return
	}
	select {}
}
