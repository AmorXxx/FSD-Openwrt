package main

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type deviceScope struct {
	Mode      string   `yaml:"mode" json:"mode"`
	Addresses []string `yaml:"addresses" json:"addresses"`
}

func (s *deviceScope) validate() error {
	if s.Mode == "" {
		s.Mode = "all"
	}
	if s.Mode != "all" && s.Mode != "selected" {
		return errors.New("device_scope.mode must be all or selected")
	}
	if len(s.Addresses) > 64 || s.Mode == "selected" && len(s.Addresses) == 0 {
		return errors.New("selected mode requires 1–64 device IP addresses")
	}
	normalized := make([]string, 0, len(s.Addresses))
	seen := map[netip.Addr]bool{}
	for _, raw := range s.Addresses {
		ip, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil || ip.Zone() != "" {
			return errors.New("device_scope requires plain IPv4/IPv6 addresses, without prefixes or zones")
		}
		ip = ip.Unmap()
		if ip.IsLoopback() || !(ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast()) || seen[ip] {
			return errors.New("invalid or duplicate device address")
		}
		seen[ip] = true
		normalized = append(normalized, ip.String())
	}
	s.Addresses = normalized
	return nil
}
func (s deviceScope) selected(addr netip.Addr) bool {
	if s.Mode != "selected" {
		return true
	}
	return slices.Contains(s.Addresses, addr.Unmap().WithZone("").String())
}
func (app *application) scopedClient(s deviceScope, host string) bool {
	addr, err := netip.ParseAddr(escapedDomain(host))
	if err != nil {
		return s.Mode != "selected"
	}
	// Router-local self checks do not impersonate a LAN device.
	if addr.IsLoopback() || addr.Unmap() == app.lanAddr {
		return true
	}
	return s.selected(addr)
}
func sameScope(a, b deviceScope) bool {
	return a.Mode == b.Mode && slices.Equal(a.Addresses, b.Addresses)
}
func scopeElements(s deviceScope, v4 bool) string {
	var out []string
	for _, v := range s.Addresses {
		a, _ := netip.ParseAddr(v)
		if a.Is4() == v4 {
			out = append(out, v)
		}
	}
	return strings.Join(out, ", ")
}
func scopeSets(s deviceScope, table, prefix string) string {
	var out strings.Builder
	for _, family := range []struct {
		suffix, kind string
		v4           bool
	}{{"4", "ipv4_addr", true}, {"6", "ipv6_addr", false}} {
		name := prefix + family.suffix
		fmt.Fprintf(&out, "add set inet %s %s { type %s; }\nflush set inet %s %s\n", table, name, family.kind, table, name)
		if values := scopeElements(s, family.v4); values != "" {
			fmt.Fprintf(&out, "add element inet %s %s { %s }\n", table, name, values)
		}
	}
	return out.String()
}
func (app *application) guardRules(s deviceScope, monitorOnly ...bool) []string {
	if len(monitorOnly) > 0 && monitorOnly[0] {
		return nil
	}
	var rules []string
	if s.Mode == "selected" {
		rules = append(rules, "ip saddr != @fsd_devices4 return", "ip6 saddr != @fsd_devices6 return")
	}
	rules = append(rules, fmt.Sprintf("iifname %q oifname != %q iifname @fsd_forward_ready return", app.lanDevice, app.lanDevice), fmt.Sprintf("iifname %q oifname != %q counter drop", app.lanDevice, app.lanDevice))
	return rules
}
func (app *application) bypassRules(s deviceScope, monitorOnly ...bool) []string {
	if len(monitorOnly) > 0 && monitorOnly[0] {
		return nil
	}
	if s.Mode != "selected" {
		return nil
	}
	var rules []string
	for _, f := range []struct{ proto, set string }{{"ip", "fsd_devices4"}, {"ip6", "fsd_devices6"}} {
		rules = append(rules, fmt.Sprintf("iifname %q %s saddr != @%s meta l4proto { tcp, udp } th dport 53 redirect to :%d", app.lanDevice, f.proto, f.set, app.resolverPort))
	}
	return rules
}
func (app *application) guardFile(s deviceScope, monitorOnly ...bool) []byte {
	var out strings.Builder
	out.WriteString("# Generated from device_scope; do not edit.\nset fsd_forward_ready { type ifname; flags timeout; }\n")
	for _, f := range []struct {
		suffix, kind string
		v4           bool
	}{{"4", "ipv4_addr", true}, {"6", "ipv6_addr", false}} {
		fmt.Fprintf(&out, "set fsd_devices%s { type %s;", f.suffix, f.kind)
		if v := scopeElements(s, f.v4); v != "" {
			fmt.Fprintf(&out, " elements = { %s };", v)
		}
		out.WriteString(" }\n")
	}
	out.WriteString("chain fsd_forward_guard { type filter hook forward priority -20; policy accept;\n")
	for _, r := range app.guardRules(s, monitorOnly...) {
		out.WriteString(r + "\n")
	}
	out.WriteString("}\n")
	out.WriteString("chain fsd_dns_bypass { type nat hook prerouting priority -110; policy accept;\n")
	for _, r := range app.bypassRules(s, monitorOnly...) {
		out.WriteString(r + "\n")
	}
	out.WriteString("}\n")
	return []byte(out.String())
}
func (app *application) liveGuard(s deviceScope, monitorOnly ...bool) string {
	out := scopeSets(s, "fw4", "fsd_devices") + "flush chain inet fw4 fsd_forward_guard\n"
	for _, r := range app.guardRules(s, monitorOnly...) {
		out += "add rule inet fw4 fsd_forward_guard " + r + "\n"
	}
	out += "add chain inet fw4 fsd_dns_bypass { type nat hook prerouting priority -110; policy accept; }\nflush chain inet fw4 fsd_dns_bypass\n"
	for _, r := range app.bypassRules(s, monitorOnly...) {
		out += "add rule inet fw4 fsd_dns_bypass " + r + "\n"
	}
	return out
}

// Keep the boot rules, saved YAML and live rules together; nft batches are atomic.
func (app *application) commitSettings(next *runtimeConfig, applyChanges func() error) (err error) {
	previous := app.current.Load()
	file := filepath.Join(app.dir, "config.yaml")
	if info, e := os.Lstat(file); e == nil && info.Mode()&os.ModeSymlink != 0 {
		name, e := app.activeProfile()
		if e != nil {
			return e
		}
		file = app.profileFile(name)
	}
	old, readErr := os.ReadFile(file)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	content, err := yaml.Marshal(next.config)
	if err != nil {
		return err
	}
	scopeChanged := app.firewallEnabled && (!sameScope(previous.Scope, next.Scope) || previous.MonitorOnly != next.MonitorOnly)
	guard := "/etc/nftables.d/45-fsd-forward-guard.nft"
	var oldGuard []byte
	var script string
	if scopeChanged {
		script = app.filterScript(next.config)
		if app.guardEnabled {
			oldGuard, err = os.ReadFile(guard)
			if err != nil {
				return err
			}
			script += app.liveGuard(next.Scope, next.MonitorOnly)
		}
		if err = nftWithMode(script, true); err != nil {
			return err
		}
	}
	rollback := func() error {
		var errorsFound []error
		if readErr == nil {
			errorsFound = append(errorsFound, writePrivate(file, old))
		} else {
			errorsFound = append(errorsFound, os.Remove(file))
		}
		if oldGuard != nil {
			errorsFound = append(errorsFound, writePrivate(guard, oldGuard))
		}
		return errors.Join(errorsFound...)
	}

	if oldGuard != nil {
		if err = writePrivate(guard, app.guardFile(next.Scope, next.MonitorOnly)); err != nil {
			return err
		}
	}
	if err = writePrivate(file, content); err != nil {
		return errors.Join(err, rollback())
	}
	if scopeChanged {
		if err = nft(script); err != nil {
			return errors.Join(err, rollback())
		}
	}
	if applyChanges != nil {
		if err = applyChanges(); err != nil {
			err = errors.Join(err, rollback())
			if scopeChanged {
				back := app.filterScript(previous.config)
				if app.guardEnabled {
					back += app.liveGuard(previous.Scope, previous.MonitorOnly)
				}
				err = errors.Join(err, nft(back))
			}
			return err
		}
	}
	app.current.Store(next)
	if scopeChanged {
		// Existing DNS NAT bindings must not retain the previous device scope.
		for _, family := range []string{"ipv4", "ipv6"} {
			for _, protocol := range []string{"udp", "tcp"} {
				_, _ = command(3*time.Second, "conntrack", "-D", "-f", family, "-p", protocol, "--dport", strconv.Itoa(app.dnsPort))
			}
		}
	}
	return nil
}
