package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
)

type editableConfig struct {
	Sites          []siteConfig `json:"h2_sites"`
	DNSRules       []dnsRule    `json:"dns_rules"`
	DNSPassthrough []string     `json:"-"`
	MonitorOnly    bool         `json:"monitor_only"`
	Scope          deviceScope  `json:"device_scope"`
	Blacklist      dnsBlacklist `json:"-"`
	DNS            []dnsRecord  `json:"-"`
}

func editableSections(cfg config) editableConfig {
	return editableConfig{Sites: append([]siteConfig{}, cfg.Sites...), DNSRules: cfg.DNSRules, DNSPassthrough: append([]string{}, cfg.DNSPassthrough...), MonitorOnly: cfg.MonitorOnly, Scope: deviceScope{Mode: cfg.Scope.Mode, Addresses: append([]string{}, cfg.Scope.Addresses...)}, Blacklist: dnsBlacklist{Block: append([]string{}, cfg.Blacklist.Block...), Allow: append([]string{}, cfg.Blacklist.Allow...)}, DNS: append([]dnsRecord{}, cfg.DNS...)}
}

func (cfg editableConfig) revision() string {
	data, _ := json.Marshal(cfg)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

type jsonField struct {
	Name  string `json:"name"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Keep each value as JSON text so large integers and nested values survive editing.
func objectFields(value string) []jsonField {
	decoder := json.NewDecoder(strings.NewReader(value))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil
	}
	fields := []jsonField{}
	seen := make(map[string]bool)
	for decoder.More() {
		start := decoder.InputOffset()
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] || strings.ContainsAny(name, "\x00\r\n") || len(fields) >= 80 {
			return nil
		}
		key := strings.TrimLeft(value[start:decoder.InputOffset()], ", \r\n\t")
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil
		}
		seen[name] = true
		fields = append(fields, jsonField{Name: name, Key: key, Value: string(raw)})
	}
	return fields
}

type editableRoute struct {
	ResponseMode string      `json:"response_mode"`
	Path         string      `json:"path"`
	Fields       []jsonField `json:"fields"`
	JSON         string      `json:"json,omitempty"`
}

type editorSnapshot struct {
	Sites          []editableSite `json:"h2_sites"`
	DNSRules       []dnsRule      `json:"dns_rules"`
	DNSPassthrough []string       `json:"-"`
	MonitorOnly    bool           `json:"monitor_only"`
	Scope          deviceScope    `json:"device_scope"`
	Blacklist      dnsBlacklist   `json:"-"`
	Revision       string         `json:"revision"`
	DNS            []dnsRecord    `json:"-"`
}

type editableSite struct {
	RequestClientCert bool            `json:"request_client_cert"`
	Domain            string          `json:"domain"`
	Certificate       string          `json:"cert"`
	Key               string          `json:"key"`
	Routes            []editableRoute `json:"h2_routes"`
}

func snapshotForEditor(cfg config) editorSnapshot {
	sections := editableSections(cfg)
	sites := make([]editableSite, 0, len(cfg.Sites))
	for _, site := range cfg.Sites {
		out := editableSite{RequestClientCert: site.RequestClientCert, Domain: site.Domain, Certificate: site.Certificate, Key: site.Key, Routes: []editableRoute{}}
		for _, route := range site.Routes {
			item := editableRoute{ResponseMode: route.ResponseMode, Path: route.Path, Fields: objectFields(route.JSON)}
			if item.Fields == nil {
				item.JSON = route.JSON
			}
			out.Routes = append(out.Routes, item)
		}
		sites = append(sites, out)
	}
	return editorSnapshot{Sites: sites, DNSRules: append([]dnsRule{}, cfg.DNSRules...), MonitorOnly: cfg.MonitorOnly, Scope: sections.Scope, Revision: sections.revision()}
}

type editorUpdate struct {
	Sites          *[]editorSiteUpdate `json:"h2_sites,omitempty"`
	DNSRules       *[]dnsRule          `json:"dns_rules,omitempty"`
	DNSPassthrough *[]string           `json:"-"`
	Scope          *deviceScope        `json:"device_scope,omitempty"`
	Blacklist      *dnsBlacklist       `json:"-"`
	Revision       string              `json:"revision"`
	DNS            []dnsRecord         `json:"-"`
	Routes         []editorRouteUpdate `json:"-"`
}

type editorSiteUpdate struct {
	RequestClientCert bool                `json:"request_client_cert"`
	Domain            string              `json:"domain"`
	Certificate       string              `json:"cert"`
	Key               string              `json:"key"`
	Source            string              `json:"source,omitempty"`
	Routes            []editorRouteUpdate `json:"h2_routes"`
}

func (input editorUpdate) updatedSites(previous []siteConfig) ([]siteConfig, error) {
	if input.Sites == nil {
		return nil, errors.New("缺少 h2_sites 列表")
	}
	sites := make([]siteConfig, 0, len(*input.Sites))
	for _, item := range *input.Sites {
		if item.Routes == nil {
			return nil, fmt.Errorf("站点 %s 缺少 h2_routes 列表；无接口请填写 []", item.Domain)
		}
		source := domainName(item.Source)
		if source == "" {
			source = domainName(item.Domain)
		}
		var old []customRoute
		for _, site := range previous {
			if site.Domain == source {
				old = site.Routes
				break
			}
		}
		routes, err := (editorUpdate{Routes: item.Routes}).updatedRoutes(old)
		if err != nil {
			return nil, fmt.Errorf("站点 %s: %w", item.Domain, err)
		}
		sites = append(sites, siteConfig{RequestClientCert: item.RequestClientCert, Domain: item.Domain, Certificate: item.Certificate, Key: item.Key, Routes: routes})
	}
	return sites, nil
}

type editorRouteUpdate struct {
	ResponseMode *string `json:"response_mode,omitempty"`
	Path         string  `json:"path"`
	JSON         string  `json:"json"`
	Source       string  `json:"source"`
}

func (input editorUpdate) updatedRoutes(previous []customRoute) ([]customRoute, error) {
	routes := make([]customRoute, 0, len(input.Routes))
	for _, item := range input.Routes {
		raw := item.JSON
		mode := ""
		if item.Source != "" {
			index := slices.IndexFunc(previous, func(route customRoute) bool { return route.Path == item.Source })
			if index < 0 || raw != "" {
				return nil, errors.New("无效的现有接口引用")
			}
			raw = previous[index].JSON
			mode = previous[index].ResponseMode
		}
		if item.ResponseMode != nil {
			mode = *item.ResponseMode
		}
		routes = append(routes, customRoute{Path: item.Path, JSON: raw, ResponseMode: mode})
	}
	return routes, nil
}

func (app *application) editorSnapshot(cfg config) editorSnapshot {
	snapshot := snapshotForEditor(cfg)
	if name, err := app.activeProfile(); err == nil {
		snapshot.Revision += ":" + name
	}
	return snapshot
}

func (app *application) saveEditor(input editorUpdate) (editorSnapshot, error) {
	app.update.Lock()
	defer app.update.Unlock()

	previous := app.current.Load()
	if input.Revision != app.editorSnapshot(previous.config).Revision {
		return editorSnapshot{}, errors.New("设备范围、DNS、黑白名单或接口配置已更新，请重新读取配置后再编辑")
	}
	if (input.DNS == nil && input.DNSRules == nil) || input.Sites == nil {
		return editorSnapshot{}, errors.New("缺少 DNS 或 h2_sites 列表")
	}
	sites, err := input.updatedSites(previous.Sites)
	if err != nil {
		return editorSnapshot{}, err
	}
	cfg := previous.config
	cfg.DNS, cfg.Sites = input.DNS, sites
	if input.DNSPassthrough != nil {
		cfg.DNSPassthrough = slices.Clone(*input.DNSPassthrough)
	} else {
		cfg.DNSPassthrough = slices.Clone(cfg.DNSPassthrough)
	}
	if input.Scope != nil {
		cfg.Scope = *input.Scope
	}
	// Omitted by older editors: preserve the existing persistent rules.
	if input.Blacklist != nil {
		if input.Blacklist.Block == nil || input.Blacklist.Allow == nil {
			return editorSnapshot{}, errors.New("dns_blacklist 必须包含 block 和 allow 列表；清空请使用空列表")
		}
		cfg.Blacklist = *input.Blacklist
	}
	// Validation normalizes lists in place. The active settings must stay immutable.
	if input.DNSRules != nil {
		cfg.setDNSRules(*input.DNSRules)
	}
	cfg.Blacklist.Block = slices.Clone(cfg.Blacklist.Block)
	cfg.Blacklist.Allow = slices.Clone(cfg.Blacklist.Allow)
	if err := cfg.validate(); err != nil {
		return editorSnapshot{}, err
	}
	if reflect.DeepEqual(cfg.Sites, previous.Sites) && (cfg.DNSRules == nil) == (previous.DNSRules == nil) && slices.Equal(cfg.DNSRules, previous.DNSRules) && slices.Equal(cfg.DNSPassthrough, previous.DNSPassthrough) && sameScope(cfg.Scope, previous.Scope) && slices.Equal(cfg.DNS, previous.DNS) &&
		slices.Equal(cfg.Blacklist.Block, previous.Blacklist.Block) && slices.Equal(cfg.Blacklist.Allow, previous.Blacklist.Allow) {
		return app.editorSnapshot(previous.config), nil
	}
	settings := app.runtimeSettings(cfg, previous.tls, previous.certificates)
	if !sameSiteCertificates(cfg.Sites, previous.Sites) {
		settings, err = app.prepare(cfg)
		if err != nil {
			return editorSnapshot{}, err
		}
	}
	if err := app.commitSettings(settings, nil); err != nil {
		return editorSnapshot{}, err
	}
	app.monitor.message("已更新设备范围、DNS、持久黑白名单和 HTTPS JSON 接口配置")
	return app.editorSnapshot(cfg), nil
}

func (app *application) configEditor(writer http.ResponseWriter, req *http.Request) {
	writeSnapshot := func(snapshot editorSnapshot) {
		writer.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(writer)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(snapshot)
	}
	switch req.Method {
	case http.MethodGet:
		writeSnapshot(app.editorSnapshot(app.current.Load().config))
	case http.MethodPut:
		var input editorUpdate
		decoder := json.NewDecoder(http.MaxBytesReader(writer, req.Body, 64*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || decoder.Decode(new(json.RawMessage)) != io.EOF {
			http.Error(writer, "无效的配置编辑请求", 400)
			return
		}
		snapshot, err := app.saveEditor(input)
		if err != nil {
			http.Error(writer, err.Error(), 400)
			return
		}
		writeSnapshot(snapshot)
	default:
		http.Error(writer, "use GET or PUT", 405)
	}
}

func sameSiteCertificates(a, b []siteConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Domain != b[i].Domain || a[i].Certificate != b[i].Certificate || a[i].Key != b[i].Key {
			return false
		}
	}
	return true
}
