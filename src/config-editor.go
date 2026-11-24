package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

type editableConfig struct {
	Scope     deviceScope   `json:"device_scope"`
	Blacklist dnsBlacklist  `json:"dns_blacklist"`
	DNS       []dnsRecord   `json:"dns"`
	Routes    []customRoute `json:"h2_routes"`
}

func editableSections(cfg config) editableConfig {
	return editableConfig{Scope: deviceScope{Mode: cfg.Scope.Mode, Addresses: append([]string{}, cfg.Scope.Addresses...)}, Blacklist: dnsBlacklist{Block: append([]string{}, cfg.Blacklist.Block...), Allow: append([]string{}, cfg.Blacklist.Allow...)}, DNS: append([]dnsRecord{}, cfg.DNS...), Routes: append([]customRoute{}, cfg.Routes...)}
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
	Path   string      `json:"path"`
	Fields []jsonField `json:"fields"`
	JSON   string      `json:"json,omitempty"`
}

type editorSnapshot struct {
	Scope     deviceScope     `json:"device_scope"`
	Blacklist dnsBlacklist    `json:"dns_blacklist"`
	Revision  string          `json:"revision"`
	DNS       []dnsRecord     `json:"dns"`
	Routes    []editableRoute `json:"h2_routes"`
}

func snapshotForEditor(cfg config) editorSnapshot {
	sections := editableSections(cfg)
	routes := make([]editableRoute, 0, len(sections.Routes))
	for _, route := range sections.Routes {
		item := editableRoute{Path: route.Path, Fields: objectFields(route.JSON)}
		if item.Fields == nil {
			item.JSON = route.JSON
		}
		routes = append(routes, item)
	}
	return editorSnapshot{Scope: sections.Scope, Blacklist: sections.Blacklist, Revision: sections.revision(), DNS: sections.DNS, Routes: routes}
}

type editorUpdate struct {
	Scope     *deviceScope        `json:"device_scope,omitempty"`
	Blacklist *dnsBlacklist       `json:"dns_blacklist,omitempty"`
	Revision  string              `json:"revision"`
	DNS       []dnsRecord         `json:"dns"`
	Routes    []editorRouteUpdate `json:"h2_routes"`
}

type editorRouteUpdate struct {
	Path   string `json:"path"`
	JSON   string `json:"json"`
	Source string `json:"source"`
}

func (input editorUpdate) updatedRoutes(previous []customRoute) ([]customRoute, error) {
	routes := make([]customRoute, 0, len(input.Routes))
	for _, item := range input.Routes {
		raw := item.JSON
		if item.Source != "" {
			index := slices.IndexFunc(previous, func(route customRoute) bool { return route.Path == item.Source })
			if index < 0 || raw != "" {
				return nil, errors.New("无效的现有接口引用")
			}
			raw = previous[index].JSON
		}
		routes = append(routes, customRoute{Path: item.Path, JSON: raw})
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
	if input.DNS == nil || input.Routes == nil {
		return editorSnapshot{}, errors.New("缺少 DNS 或 h2_routes 列表")
	}
	routes, err := input.updatedRoutes(previous.Routes)
	if err != nil {
		return editorSnapshot{}, err
	}
	cfg := previous.config
	cfg.DNS, cfg.Routes = input.DNS, routes
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
	cfg.Blacklist.Block = slices.Clone(cfg.Blacklist.Block)
	cfg.Blacklist.Allow = slices.Clone(cfg.Blacklist.Allow)
	if err := cfg.validate(); err != nil {
		return editorSnapshot{}, err
	}
	if sameScope(cfg.Scope, previous.Scope) && slices.Equal(cfg.DNS, previous.DNS) && slices.Equal(cfg.Routes, previous.Routes) &&
		slices.Equal(cfg.Blacklist.Block, previous.Blacklist.Block) && slices.Equal(cfg.Blacklist.Allow, previous.Blacklist.Allow) {
		return app.editorSnapshot(previous.config), nil
	}
	if err := app.commitSettings(app.runtimeSettings(cfg, previous.tls, previous.certificates), nil); err != nil {
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
