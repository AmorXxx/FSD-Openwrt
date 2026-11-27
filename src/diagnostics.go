package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const diagnosticPublicName = "www.iana.org"

type protocolResult struct {
	Domain     string `json:"domain"`
	Expected   string `json:"expected"`
	Protocol   string `json:"protocol,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	TLSOnly    bool   `json:"tls_only,omitempty"`
}

type diagnosticResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func checked(name string, passed bool, detail string) diagnosticResult {
	status := "fail"
	if passed {
		status = "pass"
	}
	return diagnosticResult{Name: name, Status: status, Detail: detail}
}

func skipped(name, detail string) diagnosticResult {
	return diagnosticResult{Name: name, Status: "skip", Detail: detail}
}

func diagnosticClient(version string, current *runtimeConfig) *http.Client {
	return diagnosticSiteClient(version, diagnosticSiteDomain(current, current.primarySite().Domain), current.tls)
}

func diagnosticSiteClient(version, domain string, pair *tls.Certificate) *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP2(version == "h2")
	protocols.SetHTTP1(version == "http/1.1")
	return &http.Client{
		Timeout: 4 * time.Second,
		Transport: &http.Transport{
			Protocols: protocols, DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{
				ServerName: domain, MinVersion: tls.VersionTLS12,
				InsecureSkipVerify: true,
				VerifyConnection: func(state tls.ConnectionState) error {
					// This probe trusts only the certificate currently loaded by FSD.
					if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, pair.Certificate[0]) {
						return errors.New("TLS certificate does not match the running service")
					}
					return nil
				},
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (app *application) diagnosticURL(path string) string {
	target := url.URL{Scheme: "https", Host: net.JoinHostPort(app.lanAddr.String(), strconv.Itoa(app.sitePort)), Path: path}
	return target.String()
}

func (app *application) protocolProbe(ctx context.Context, current *runtimeConfig) []protocolResult {
	result := make([]protocolResult, 0, 2)
	for _, site := range current.sites() {
		host := diagnosticSiteDomain(current, site.Domain)
		for _, version := range []string{"h2", "http/1.1"} {
			req, _ := http.NewRequestWithContext(ctx, http.MethodHead, app.diagnosticURL("/"), nil)
			req.Host = host
			probe := protocolResult{Expected: version, Domain: site.Domain}
			if route, found := matchingRoute(site.Routes, "/"); found && route.ResponseMode == "close" {
				probe.TLSOnly = true
				client := diagnosticSiteClient(version, host, current.certificates[site.Domain])
				cfg := client.Transport.(*http.Transport).TLSClientConfig.Clone()
				cfg.NextProtos = []string{version}
				dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 4 * time.Second}, Config: cfg}
				conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(app.lanAddr.String(), strconv.Itoa(app.sitePort)))
				if err != nil {
					probe.Error = err.Error()
				} else {
					probe.Protocol = conn.(*tls.Conn).ConnectionState().NegotiatedProtocol
					// An absent ALPN negotiation falls back to HTTP/1.1, never HTTP/2.
					if probe.Protocol == "" && version == "http/1.1" {
						probe.Protocol = "http/1.1"
					}
					probe.OK = probe.Protocol == version
					if !probe.OK {
						probe.Error = "TLS ALPN 协议与预期不符"
					}
					conn.Close()
				}
				result = append(result, probe)
				continue
			}
			response, err := diagnosticSiteClient(version, host, current.certificates[site.Domain]).Do(req)
			if err != nil {
				probe.Error = err.Error()
			} else {
				response.Body.Close()
				probe.HTTPStatus = response.StatusCode
				probe.Protocol = strings.ToLower(response.Proto)
				if response.ProtoMajor == 2 && response.TLS != nil && response.TLS.NegotiatedProtocol == "h2" {
					probe.Protocol = "h2"
				}
				probe.OK = probe.Protocol == version && (response.StatusCode == http.StatusOK || response.StatusCode == http.StatusNotFound)
				if !probe.OK {
					probe.Error = "协议或响应与预期不符"
				}
			}
			result = append(result, probe)
		}
	}
	return result
}

func (app *application) checkRoutes(ctx context.Context, current *runtimeConfig) []diagnosticResult {
	if len(current.Sites) == 0 {
		return []diagnosticResult{skipped("自定义 JSON 接口", "当前没有配置接口")}
	}

	results := []diagnosticResult{}
	for _, site := range current.Sites {
		host := diagnosticSiteDomain(current, site.Domain)
		client := diagnosticSiteClient("h2", host, current.certificates[site.Domain])
		if len(site.Routes) == 0 {
			results = append(results, skipped(site.Domain, "当前站点没有配置接口"))
		}
		for _, route := range site.Routes {
			name := "JSON " + site.Domain + " " + route.Path
			if route.ResponseMode == "close" {
				results = append(results, skipped(name, "记录后终止请求模式：不执行 HTTP 200/JSON 应答检查"))
				continue
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, app.diagnosticURL(diagnosticRoutePath(route.Path, site.Routes)), nil)
			req.Host = host
			response, err := client.Do(req)
			if err != nil {
				results = append(results, checked(name, false, err.Error()))
				continue
			}
			body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
			response.Body.Close()
			checksum := fmt.Sprintf("%x", md5.Sum(body))
			ok := err == nil && response.StatusCode == http.StatusOK && bytes.Equal(body, []byte(route.JSON)) &&
				response.Header.Get("Content-Type") == "application/json" && response.Header.Get("X-Checksum") == checksum
			detail := "HTTP 200、JSON 正文和 X-Checksum 一致"
			if !ok {
				detail = fmt.Sprintf("HTTP %d，响应正文、类型或校验值与配置不一致", response.StatusCode)
			}
			results = append(results, checked(name, ok, detail))
		}
	}
	return results
}

func (app *application) diagnosticDNS(ctx context.Context, transport, name string) (*dns.Msg, error) {
	resolver := dns.Client{Net: transport, Timeout: 3500 * time.Millisecond}
	query := new(dns.Msg)
	query.SetQuestion(dns.Fqdn(name), dns.TypeA)
	response, _, err := resolver.ExchangeContext(ctx, query, net.JoinHostPort("127.0.0.1", strconv.Itoa(app.dnsPort)))
	return response, err
}

func checkDNSResponse(name string, reply *dns.Msg, err error, wantCode int, wantIP net.IP) diagnosticResult {
	if err != nil {
		return checked(name, false, err.Error())
	}
	if reply == nil || reply.Rcode != wantCode {
		code := "空响应"
		if reply != nil {
			code = dns.RcodeToString[reply.Rcode]
		}
		return checked(name, false, "返回 "+code+"，预期 "+dns.RcodeToString[wantCode])
	}
	if wantCode != dns.RcodeSuccess {
		return checked(name, true, "返回 "+dns.RcodeToString[wantCode])
	}
	for _, answer := range reply.Answer {
		if record, ok := answer.(*dns.A); ok && (wantIP == nil || record.A.Equal(wantIP)) {
			return checked(name, true, "返回 A "+record.A.String())
		}
	}
	return checked(name, false, "未返回预期 A 记录")
}

func (app *application) checkDNS(ctx context.Context, current *runtimeConfig) []diagnosticResult {
	temporary := app.monitor.temporaryRules()
	var results []diagnosticResult
	for _, site := range current.sites() {
		host := diagnosticSiteDomain(current, site.Domain)
		if current.passthroughDNS(host) && !blockedName(host, current.config, temporary) {
			results = append(results, skipped("DNS · "+site.Domain, "测试域名命中 DNS 直通例外，使用上游解析结果，不校验固定 IP"))
			continue
		}
		for _, transport := range []string{"udp", "tcp"} {
			name := "DNS " + strings.ToUpper(transport) + " · " + site.Domain
			reply, err := app.diagnosticDNS(ctx, transport, host)
			code := dns.RcodeSuccess
			if blockedName(host, current.config, temporary) {
				code = dns.RcodeNameError
			}
			expectedIP, _ := current.dnsRecord(host)
			results = append(results, checkDNSResponse(name, reply, err, code, expectedIP))
		}
	}
	if blockedName(diagnosticPublicName, current.config, temporary) {
		results = append(results, skipped("公网 DNS 转发", "测试域名命中黑名单"))
	} else {
		reply, err := app.diagnosticDNS(ctx, "udp", diagnosticPublicName)
		results = append(results, checkDNSResponse("公网 DNS 转发", reply, err, dns.RcodeSuccess, nil))
	}
	const testName = "DNS 黑名单"
	if current.MonitorOnly {
		return append(results, skipped(testName, "仅监控模式不执行黑名单，包括临时规则"))
	}
	replace := strings.NewReplacer("*", "fsd-check", "?", "a")
	blockRules := append(slices.Clone(temporary), current.Blacklist.Block...)
	for _, r := range current.DNSRules {
		if r.Action == "block" {
			blockRules = append(blockRules, r.Name)
		}
	}
	for _, rule := range blockRules {
		domain := replace.Replace(rule)
		if validDomain(domain, false) && blockedName(domain, current.config, temporary) {
			reply, err := app.diagnosticDNS(ctx, "udp", domain)
			return append(results, checkDNSResponse(testName, reply, err, dns.RcodeNameError, nil))
		}
	}
	return append(results, skipped(testName, "当前无可验证的拦截规则"))
}

func (app *application) checkSIM(ctx context.Context, current *runtimeConfig) []diagnosticResult {
	encoded, err := exec.CommandContext(ctx, "ubus", "call", "network.interface."+app.modemInterface, "status").Output()
	modem := struct {
		Up     bool   `json:"up"`
		Device string `json:"l3_device"`
	}{}
	if err != nil || json.Unmarshal(encoded, &modem) != nil || !modem.Up || modem.Device == "" {
		return []diagnosticResult{checked("SIM 上游", false, "调制解调器接口未就绪")}
	}

	results := make([]diagnosticResult, 2)
	var wait sync.WaitGroup
	for index, family := range []string{"4", "6"} {
		wait.Go(func() {
			name := "SIM IPv" + family + " 外网"
			route, err := exec.CommandContext(ctx, "ip", "-"+family, "route", "show", "default", "dev", modem.Device).Output()
			if err != nil {
				results[index] = checked(name, false, "无法读取默认路由")
				return
			}
			if len(bytes.TrimSpace(route)) == 0 {
				results[index] = checked(name, false, "SIM 接口没有 IPv"+family+" 默认路由")
				if family == "6" {
					results[index] = skipped(name, "SIM 接口没有 IPv6 默认路由")
				}
				return
			}
			code, err := exec.CommandContext(ctx, "curl", "-"+family, "--noproxy", "*", "--interface", modem.Device,
				"--connect-timeout", "3", "--max-time", "5", "-sS", "-o", "/dev/null", "-w", "%{http_code}",
				"https://"+diagnosticPublicName+"/").Output()
			if err != nil {
				results[index] = checked(name, false, "经 "+modem.Device+" 访问 "+diagnosticPublicName+" 失败")
				return
			}
			results[index] = checked(name, string(code) == "200", modem.Device+" · "+diagnosticPublicName+" · HTTP "+string(code))
		})
	}
	wait.Wait()
	return results
}

func hotspotInterfaces(output string) []string {
	var current string
	var aps []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "Interface":
			current = fields[1]
		case "type":
			if fields[1] == "AP" && current != "" {
				aps = append(aps, current)
			}
		}
	}
	return aps
}

func (app *application) checkHotspot(ctx context.Context) diagnosticResult {
	data, err := exec.CommandContext(ctx, "iw", "dev").Output()
	if err != nil {
		return checked("Wi-Fi 热点", false, "无法读取无线接口")
	}
	var active []string
	for _, candidate := range hotspotInterfaces(string(data)) {
		device, err := net.InterfaceByName(candidate)
		if err == nil && device.Flags&net.FlagUp != 0 {
			active = append(active, candidate)
		}
	}
	if len(active) == 0 {
		return checked("Wi-Fi 热点", false, "没有处于 UP 状态的 AP 接口")
	}
	stations := 0
	for _, name := range active {
		data, err := exec.CommandContext(ctx, "iw", "dev", name, "station", "dump").Output()
		if err != nil {
			return checked("Wi-Fi 热点", false, "无法读取 "+name+" 的关联状态")
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Station ") {
				stations++
			}
		}
	}
	return checked("Wi-Fi 热点", true, fmt.Sprintf("%s 已启用，%d 台关联客户端", strings.Join(active, ", "), stations))
}

func (app *application) checkDHCP() diagnosticResult {
	files, _ := filepath.Glob("/var/etc/dnsmasq.conf.*")
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok || (key != "dhcp-option" && key != "dhcp-option-force") {
				continue
			}
			fields := strings.Split(value, ",")
			if len(fields) > 2 && strings.TrimPrefix(fields[0], "tag:") == "lan" &&
				(fields[1] == "6" || fields[1] == "option:dns-server") && slices.Contains(fields[2:], app.lanAddr.String()) {
				return checked("DHCP DNS 选项", true, "dnsmasq 运行配置下发 "+app.lanAddr.String())
			}
		}
	}
	return checked("DHCP DNS 选项", false, "dnsmasq 运行配置未找到 LAN DNS "+app.lanAddr.String())
}

func (app *application) checkLocal(ctx context.Context) []diagnosticResult {
	results := []diagnosticResult{app.checkHotspot(ctx), app.checkDHCP()}
	app.monitor.Lock()
	enabled, capture := app.monitor.enabled, app.monitor.packetCapture
	app.monitor.Unlock()
	switch {
	case !enabled:
		results = append(results, skipped("连接监控", "记录已暂停"))
	default:
		flows, err := connectionsFromKernel()
		detail := fmt.Sprintf("读到 %d 条内核连接，数据包监控已启动", len(flows))
		if err != nil || !capture {
			detail = "内核连接读取或数据包监控不可用"
		}
		results = append(results, checked("连接监控", err == nil && capture, detail))
	}
	for _, service := range []string{"fsd-service", "uhttpd"} {
		err := exec.CommandContext(ctx, "/etc/init.d/"+service, "enabled").Run()
		detail := "已启用"
		if err != nil {
			detail = "未启用或无法读取"
		}
		results = append(results, checked(service+" 开机自启", err == nil, detail))
	}
	return results
}

func (app *application) runDiagnostics(parent context.Context, group string) ([]diagnosticResult, error) {
	ctx, cancel := context.WithTimeout(parent, 11*time.Second)
	defer cancel()
	current := app.current.Load()
	type diagnostic func() []diagnosticResult
	tests := map[string]diagnostic{
		"guard": func() []diagnosticResult { return []diagnosticResult{app.checkForwardGuard(ctx)} },
		"https": func() []diagnosticResult {
			if current.MonitorOnly {
				return []diagnosticResult{skipped("业务 HTTPS", "仅监控模式已禁用")}
			}
			var results []diagnosticResult
			for _, probe := range app.protocolProbe(ctx, current) {
				detail := probe.Error
				if probe.OK {
					detail = fmt.Sprintf("%s · HTTP %d", probe.Protocol, probe.HTTPStatus)
					if probe.TLSOnly {
						detail = probe.Protocol + " · TLS/ALPN 验证通过；终止请求模式未检查 HTTP 应答"
					}
				}
				results = append(results, checked(probe.Domain+" · "+probe.Expected, probe.OK, detail))
			}
			return results
		},
		"routes": func() []diagnosticResult { return app.checkRoutes(ctx, current) },
		"dns":    func() []diagnosticResult { return app.checkDNS(ctx, current) },
		"sim":    func() []diagnosticResult { return app.checkSIM(ctx, current) },
		"local":  func() []diagnosticResult { return app.checkLocal(ctx) },
	}
	groups := []string{group}
	if group == "all" {
		groups = []string{"guard", "https", "routes", "dns", "sim", "local"}
	} else if tests[group] == nil {
		return nil, errors.New("未知的自检项目")
	}
	batches := make([][]diagnosticResult, len(groups))
	var wait sync.WaitGroup
	for index, group := range groups {
		wait.Go(func() { batches[index] = tests[group]() })
	}
	wait.Wait()
	return slices.Concat(batches...), nil
}
