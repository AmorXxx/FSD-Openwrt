package main

import (
	"fmt"
	"strings"
	"testing"
)

const guardSnapshotFixture = `{"nftables": [
	{"set":{"name":"unrelated","type":"ipv4_addr","elem":["192.0.2.1"]}},
	{"set":{"name":"fsd_forward_ready","type":"ifname","flags":["timeout"],"elem":%s}},
	{"chain":{"name":"fsd_forward_guard","type":"filter","hook":"forward","prio":-20,"policy":"accept"}},
	{"rule":{"chain":"fsd_forward_guard","expr":[
		{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"br-lan"}},
		{"match":{"op":"!=","left":{"meta":{"key":"oifname"}},"right":"br-lan"}},
		{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"@fsd_forward_ready"}},
		{"return":null}]}},
	{"rule":{"chain":"fsd_forward_guard","expr":[
		{"match":{"op":"==","left":{"meta":{"key":"iifname"}},"right":"br-lan"}},
		{"match":{"op":"!=","left":{"meta":{"key":"oifname"}},"right":"br-lan"}},
		{"counter":{"packets":24,"bytes":1620}},
		{"drop":null}]}}
]}`

func TestForwardGuardDiagnostic(t *testing.T) {
	closed := fmt.Sprintf(guardSnapshotFixture, "[]")
	open := fmt.Sprintf(guardSnapshotFixture, `[{"elem":{"val":"br-lan","timeout":5,"expires":0}}]`)
	tests := []struct {
		name, snapshot, status, detail string
	}{
		{"closed", closed, "pass", "当前拦截"},
		{"open under one second", open, "pass", "当前放行"},
		{"lease for different interface", strings.Replace(open, `"val":"br-lan"`, `"val":"other"`, 1), "pass", "当前拦截"},
		{"unexpected lease duration", strings.Replace(open, `"timeout":5`, `"timeout":50`, 1), "fail", "有效期异常"},
		{"missing guard", `{"nftables":[]}`, "fail", "保护规则缺失"},
		{"wrong hook", strings.Replace(open, `"hook":"forward"`, `"hook":"input"`, 1), "fail", "与预期不符"},
		{"wrong verdict", strings.Replace(open, `"drop":null`, `"accept":null`, 1), "fail", "与预期不符"},
		{"flow offloading", strings.Replace(open, `"nftables": [`, `"nftables": [{"flowtable":{"name":"ft"}},`, 1), "fail", "流量卸载"},
		{"unreadable snapshot", "{", "fail", "无法解析"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := inspectForwardGuard([]byte(test.snapshot), "br-lan")
			if result.Name != guardCheckName || result.Status != test.status || !strings.Contains(result.Detail, test.detail) {
				t.Errorf("unexpected result: %+v", result)
			}
		})
	}
}
