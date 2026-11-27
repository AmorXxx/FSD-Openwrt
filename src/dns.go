package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
)

func suffixMatch(name, suffix string) bool {
	return name == suffix || strings.HasSuffix(name, "."+suffix)
}

func blockedName(name string, cfg config, temporary []string) bool {
	if cfg.MonitorOnly {
		return false
	}
	for _, rule := range temporary {
		if suffixMatch(name, rule) {
			return true
		}
	}
	if cfg.DNSRules != nil {
		r, ok := cfg.dnsRuleFor(name)
		return ok && r.Action == "block"
	}
	for _, rule := range cfg.Blacklist.Allow {
		if name == rule {
			return false
		}
	}
	for _, rule := range cfg.Blacklist.Block {
		if strings.ContainsAny(rule, "*?") {
			match, _ := path.Match(rule, name)
			if match {
				return true
			}
		} else if suffixMatch(name, rule) {
			return true
		}
	}
	return false
}

func (app *application) serveDNS(writer dns.ResponseWriter, query *dns.Msg) {
	reply := new(dns.Msg)
	reply.SetReply(query)
	reply.RecursionAvailable = true
	if len(query.Question) != 1 || query.Opcode != dns.OpcodeQuery {
		reply.Rcode = dns.RcodeFormatError
		_ = writer.WriteMsg(reply)
		return
	}
	question := query.Question[0]
	name := domainName(question.Name)
	client, _, _ := net.SplitHostPort(writer.RemoteAddr().String())
	cfg := app.current.Load()
	temporary := app.monitor.temporaryRules()
	kind := "forwarded"
	target := !cfg.MonitorOnly && app.scopedClient(cfg.Scope, client)
	if target && blockedName(name, cfg.config, temporary) {
		kind, reply.Rcode = "blocked", dns.RcodeNameError
	} else if address, ok := cfg.dnsRecord(name); ok && target && question.Qclass == dns.ClassINET {
		kind = "local"
		reply.Authoritative = true
		switch question.Qtype {
		case dns.TypeA, dns.TypeANY:
			reply.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30},
				A:   address,
			}}
		}
	} else {
		network := "udp"
		if writer.RemoteAddr().Network() == "tcp" {
			network = "tcp"
		}
		forwarder := dns.Client{Net: network, Timeout: 3 * time.Second, UDPSize: 1232}
		forwarded, _, err := forwarder.Exchange(query, app.dnsUpstream)
		if err == nil && forwarded != nil {
			reply = forwarded
		} else {
			kind, reply.Rcode = "upstream_error", dns.RcodeServerFailure
		}
	}
	addresses := make([]string, 0, len(reply.Answer))
	for _, answer := range reply.Answer {
		switch record := answer.(type) {
		case *dns.A:
			addresses = append(addresses, record.A.String())
		case *dns.AAAA:
			addresses = append(addresses, record.AAAA.String())
		}
	}
	if writer.RemoteAddr().Network() != "tcp" {
		size := dns.MinMsgSize
		if edns := query.IsEdns0(); edns != nil {
			size = min(max(int(edns.UDPSize()), size), 1232)
		}
		reply.Truncate(size)
	}
	app.monitor.recordDNS(client, name, dns.TypeToString[question.Qtype], kind, reply.Rcode, addresses)
	if err := writer.WriteMsg(reply); err != nil {
		log.Printf("DNS write failed: %v", err)
	}
}

func (app *application) filterScript(cfg config) string {
	script := fmt.Sprintf(`
add table inet fsd_service
flush table inet fsd_service
add set inet fsd_service blocked_ipv4 { type ipv4_addr; }
add set inet fsd_service blocked_ipv6 { type ipv6_addr; }
add chain inet fsd_service dns_lan { type nat hook prerouting priority dstnat + 5; policy accept; }
add rule inet fsd_service dns_lan iifname "%s" meta l4proto { tcp, udp } th dport 53 redirect to :%d
add chain inet fsd_service restrict_listeners { type filter hook input priority -15; policy accept; }
add rule inet fsd_service restrict_listeners iifname != "lo" meta l4proto { tcp, udp } th dport %d drop
add rule inet fsd_service restrict_listeners iifname != { "lo", "%s" } meta l4proto { tcp, udp } th dport { 53, %d, %d } drop
add chain inet fsd_service blocked_forward { type filter hook forward priority -15; policy accept; }
add rule inet fsd_service blocked_forward iifname "%s" ip daddr @blocked_ipv4 meta l4proto tcp reject with tcp reset
add rule inet fsd_service blocked_forward iifname "%s" ip6 daddr @blocked_ipv6 meta l4proto tcp reject with tcp reset
add rule inet fsd_service blocked_forward oifname "%s" ip saddr @blocked_ipv4 meta l4proto tcp reject with tcp reset
add rule inet fsd_service blocked_forward oifname "%s" ip6 saddr @blocked_ipv6 meta l4proto tcp reject with tcp reset
add rule inet fsd_service blocked_forward iifname "%s" ip daddr @blocked_ipv4 drop
add rule inet fsd_service blocked_forward iifname "%s" ip6 daddr @blocked_ipv6 drop
add rule inet fsd_service blocked_forward oifname "%s" ip saddr @blocked_ipv4 drop
add rule inet fsd_service blocked_forward oifname "%s" ip6 saddr @blocked_ipv6 drop
`, app.lanDevice, app.dnsPort, app.resolverPort, app.lanDevice, app.sitePort, app.adminPort, app.lanDevice, app.lanDevice, app.lanDevice, app.lanDevice,
		app.lanDevice, app.lanDevice, app.lanDevice, app.lanDevice)
	if cfg.Scope.Mode == "selected" {
		// Run source/destination scope checks before domain-IP block rules.
		script = strings.Replace(script, "add chain inet fsd_service dns_lan", scopeSets(cfg.Scope, "fsd_service", "selected_ipv")+"add chain inet fsd_service dns_lan", 1)
		for _, family := range []struct{ proto, set string }{{"ip", "selected_ipv4"}, {"ip6", "selected_ipv6"}} {
			// Resolver access is allowed only for non-target LAN clients redirected by fw4.
			script += fmt.Sprintf("insert rule inet fsd_service restrict_listeners iifname %q %s saddr != @%s ct status dnat meta l4proto { tcp, udp } th dport %d accept\n", app.lanDevice, family.proto, family.set, app.resolverPort)
			script += fmt.Sprintf("insert rule inet fsd_service blocked_forward iifname %q %s saddr != @%s return\n", app.lanDevice, family.proto, family.set)
			script += fmt.Sprintf("insert rule inet fsd_service blocked_forward oifname %q %s daddr != @%s return\n", app.lanDevice, family.proto, family.set)
		}
	}
	if cfg.MonitorOnly {
		script += fmt.Sprintf("insert rule inet fsd_service restrict_listeners meta l4proto tcp th dport %d reject with tcp reset\n", app.sitePort)
	}
	return script + app.blocklistScript(cfg)
}
func (app *application) installFilter(checkOnly bool) error {
	script := app.filterScript(app.current.Load().config)
	if app.guardEnabled && !checkOnly {
		script += app.liveGuard(app.current.Load().Scope, app.current.Load().MonitorOnly)
	}
	return nftWithMode(script, checkOnly)
}

func nft(script string) error {
	return nftWithMode(script, false)
}

func nftWithMode(script string, checkOnly bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	args := []string{"-f", "-"}
	if checkOnly {
		args = append([]string{"-c"}, args...)
	}
	command := exec.CommandContext(ctx, "nft", args...)
	command.Stdin = strings.NewReader(script)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (app *application) syncBlocklist() {
	var applied string
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for range tick.C {
		app.update.Lock()
		cfg := app.current.Load()
		next := app.blocklistScript(cfg.config)
		if next != applied {
			if err := nft(next); err != nil {
				log.Printf("apply domain blocking: %v", err)
				app.update.Unlock()
				continue
			}
			applied = next
		}
		app.update.Unlock()
	}
}

func (app *application) blocklistScript(cfg config) string {
	remembered, temporary := app.monitor.addresses()
	ipv4 := make(map[string]bool)
	ipv6 := make(map[string]bool)
	for _, learned := range remembered {
		if !blockedName(learned.Name, cfg, temporary) {
			continue
		}
		address, err := netip.ParseAddr(learned.IP)
		if err != nil || !address.IsGlobalUnicast() || address == app.lanAddr {
			continue
		}
		if address.Is4() {
			ipv4[address.String()] = true
		} else {
			ipv6[address.String()] = true
		}
	}
	var script strings.Builder
	for _, set := range []struct {
		name    string
		entries map[string]bool
	}{{"blocked_ipv4", ipv4}, {"blocked_ipv6", ipv6}} {
		fmt.Fprintf(&script, "flush set inet fsd_service %s\n", set.name)
		var list []string
		for address := range set.entries {
			list = append(list, address)
		}
		sort.Strings(list)
		if len(list) > 0 {
			fmt.Fprintf(&script, "add element inet fsd_service %s { %s }\n", set.name, strings.Join(list, ", "))
		}
	}

	return script.String()
}
