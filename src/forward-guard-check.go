package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"time"
)

const guardCheckName = "开机保护"

func (app *application) checkForwardGuard(ctx context.Context) diagnosticResult {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", "fw4").Output()
	if err != nil {
		return checked(guardCheckName, false, "无法读取 fw4 运行规则，当前状态无法确认")
	}
	return inspectForwardGuard(data, app.lanDevice, app.current.Load().Scope)
}

func inspectForwardGuard(data []byte, lan string, scopes ...deviceScope) diagnosticResult {
	scope := deviceScope{}
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	var state struct {
		Objects []struct {
			Chain *struct {
				Name     string
				Type     string
				Hook     string
				Priority int `json:"prio"`
				Policy   string
			}
			Rule *struct {
				Chain string
				Expr  []map[string]any
			}
			Set *struct {
				Name     string
				Type     string
				Flags    []string
				Elements json.RawMessage `json:"elem"`
			}
			Flowtable json.RawMessage
		} `json:"nftables"`
	}
	if err := json.Unmarshal(data, &state); err != nil || state.Objects == nil {
		return checked(guardCheckName, false, "无法解析 fw4 运行规则，当前状态无法确认")
	}

	var chainOK, setOK, leased, invalidLease, offloading bool
	var rules [][]map[string]any
	scopeSets := map[string]json.RawMessage{}
	for _, object := range state.Objects {
		if chain := object.Chain; chain != nil && chain.Name == forwardGuardChain {
			chainOK = chain.Type == "filter" && chain.Hook == "forward" &&
				chain.Priority == -20 && chain.Policy == "accept"
		}
		if rule := object.Rule; rule != nil && rule.Chain == forwardGuardChain {
			rules = append(rules, rule.Expr)
		}
		if set := object.Set; set != nil && (set.Name == "fsd_devices4" || set.Name == "fsd_devices6") {
			scopeSets[set.Name] = set.Elements
		}
		if set := object.Set; set != nil && set.Name == forwardReadySet {
			setOK = set.Type == "ifname" && slices.Contains(set.Flags, "timeout")
			var elements []struct {
				Element struct {
					Value   string `json:"val"`
					Timeout int
				} `json:"elem"`
			}
			if len(set.Elements) != 0 && json.Unmarshal(set.Elements, &elements) != nil {
				return checked(guardCheckName, false, "无法解析保护放行记录，当前状态无法确认")
			}
			for _, entry := range elements {
				if entry.Element.Value == lan {
					// The kernel omits expired entries from a set dump.
					leased = true
					invalidLease = entry.Element.Timeout != forwardReadyLeaseSeconds
				}
			}
		}
		offloading = offloading || len(object.Flowtable) != 0
	}
	if scope.Mode == "selected" {
		if len(rules) != 4 {
			return checked(guardCheckName, false, "设备范围保护规则缺失")
		}
		for i, family := range []string{"ip", "ip6"} {
			name := []string{"fsd_devices4", "fsd_devices6"}[i]
			expected := []map[string]any{
				{"match": map[string]any{"left": map[string]any{"payload": map[string]any{"protocol": family, "field": "saddr"}}, "op": "!=", "right": "@" + name}},
				{"return": nil},
			}
			raw, exists := scopeSets[name]
			var values []string
			if !exists || len(raw) > 0 && json.Unmarshal(raw, &values) != nil {
				return checked(guardCheckName, false, "设备范围地址集合异常")
			}
			wanted := []string{}
			for _, v := range scope.Addresses {
				if (strings.Contains(v, ":")) == (i == 1) {
					wanted = append(wanted, v)
				}
			}
			slices.Sort(values)
			slices.Sort(wanted)
			if !reflect.DeepEqual(rules[i], expected) || !slices.Equal(values, wanted) {
				return checked(guardCheckName, false, "设备范围与运行保护规则不一致")
			}
		}
		rules = rules[2:]
	}
	if !chainOK || !setOK || !forwardGuardRulesMatch(rules, lan) {
		return checked(guardCheckName, false, "保护规则缺失或与预期不符，当前状态无法确认")
	}
	if offloading {
		return checked(guardCheckName, false, "保护规则已加载，但 fw4 存在流量卸载，转发状态无法确认")
	}
	if invalidLease {
		return checked(guardCheckName, false, "当前放行，但放行记录的有效期异常")
	}
	if scope.Mode == "selected" {
		if leased {
			return checked(guardCheckName, true, "规则已生效 · 当前放行指定设备，其他设备不受保护规则限制")
		}
		return checked(guardCheckName, true, "规则已生效 · 当前拦截指定设备出网，其他设备不受保护规则限制")
	}
	if leased {
		return checked(guardCheckName, true, "规则已生效 · 当前放行客户端出网")
	}
	return checked(guardCheckName, true, "规则已生效 · 当前拦截客户端出网")
}

func forwardGuardRulesMatch(rules [][]map[string]any, lan string) bool {
	if len(rules) != 2 || len(rules[1]) != 4 {
		return false
	}
	matchInterface := func(key, op, value string) map[string]any {
		return map[string]any{"match": map[string]any{
			"left": map[string]any{"meta": map[string]any{"key": key}},
			"op":   op, "right": value,
		}}
	}
	lanOutbound := []map[string]any{
		matchInterface("iifname", "==", lan),
		matchInterface("oifname", "!=", lan),
	}
	allow := append(slices.Clone(lanOutbound),
		matchInterface("iifname", "==", "@"+forwardReadySet), map[string]any{"return": nil})
	_, hasCounter := rules[1][2]["counter"].(map[string]any)
	return reflect.DeepEqual(rules[0], allow) &&
		reflect.DeepEqual(rules[1][:2], lanOutbound) &&
		len(rules[1][2]) == 1 && hasCounter &&
		reflect.DeepEqual(rules[1][3], map[string]any{"drop": nil})
}
