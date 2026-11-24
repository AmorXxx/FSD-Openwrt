package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type dnsEvent struct {
	ID        uint64    `json:"id"`
	Time      time.Time `json:"time"`
	Client    string    `json:"client"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Result    string    `json:"result"`
	RCode     int       `json:"rcode"`
	Addresses []string  `json:"addresses"`
}

type learnedIP struct {
	Name, IP string
	Expires  time.Time
}

type connection struct {
	lastActive time.Time
	Client     string `json:"client"`
	Remote     string `json:"remote"`
	SourcePort string `json:"source_port"`
	Port       string `json:"port"`
	Protocol   string `json:"protocol"`
	Domain     string `json:"domain,omitempty"`
	Up         uint64 `json:"up_bytes"`
	Down       uint64 `json:"down_bytes"`
	UpRate     uint64 `json:"up_bytes_per_sec"`
	DownRate   uint64 `json:"down_bytes_per_sec"`
}

type clientDevice struct {
	IP       string `json:"ip"`
	MAC      string `json:"mac,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Up       uint64 `json:"up_bytes"`
	Down     uint64 `json:"down_bytes"`
	UpRate   uint64 `json:"up_bytes_per_sec"`
	DownRate uint64 `json:"down_bytes_per_sec"`
	DNSCount uint64 `json:"dns_queries"`
}

type monitor struct {
	sync.Mutex
	enabled       bool
	packetCapture bool
	events        []dnsEvent
	nextID        uint64
	logs          []string
	blocked       []string
	addressBook   map[string]learnedIP
	captured      map[string]learnedIP
	clients       map[string]clientDevice
	connections   map[string]connection
}

func newMonitor() *monitor {
	return &monitor{enabled: true, addressBook: make(map[string]learnedIP),
		captured: make(map[string]learnedIP), clients: make(map[string]clientDevice),
		connections: make(map[string]connection)}
}

func (m *monitor) message(message string) {
	m.Lock()
	defer m.Unlock()
	m.logs = append(m.logs, time.Now().Format(time.RFC3339)+" "+message)
	if len(m.logs) > 150 {
		m.logs = slices.Delete(m.logs, 0, 1)
	}
	log.Print(message)
}

func (m *monitor) temporaryRules() []string {
	m.Lock()
	defer m.Unlock()
	return slices.Clone(m.blocked)
}

func (m *monitor) setTemporaryRule(name string, add bool) error {
	m.Lock()
	defer m.Unlock()
	index := slices.Index(m.blocked, name)
	if !add {
		if index >= 0 {
			m.blocked = slices.Delete(m.blocked, index, index+1)
		}
		return nil
	}
	if index >= 0 {
		return nil
	}
	if len(m.blocked) >= 128 {
		return fmt.Errorf("temporary blacklist is full")
	}
	m.blocked = append(m.blocked, name)
	slices.Sort(m.blocked)
	return nil
}

func (m *monitor) addresses() ([]learnedIP, []string) {
	m.Lock()
	defer m.Unlock()
	now := time.Now()
	list := make([]learnedIP, 0, len(m.addressBook)+len(m.captured))
	for _, table := range []map[string]learnedIP{m.addressBook, m.captured} {
		for key, entry := range table {
			if now.After(entry.Expires) {
				delete(table, key)
				continue
			}
			list = append(list, entry)
		}
	}
	return list, slices.Clone(m.blocked)
}

func (m *monitor) recordDNS(client, name, kind, result string, code int, addresses []string) {
	m.Lock()
	defer m.Unlock()
	if !m.enabled {
		return
	}
	m.nextID++
	now := time.Now()
	m.events = append(m.events, dnsEvent{m.nextID, now, client, name, kind, result, code, addresses})
	if len(m.events) > 400 {
		m.events = slices.Delete(m.events, 0, 1)
	}
	if _, exists := m.clients[client]; exists || len(m.clients) < 256 {
		device := m.clients[client]
		device.IP = client
		device.DNSCount++
		m.clients[client] = device
	}
	for _, address := range addresses {
		key := client + " " + address
		if _, exists := m.addressBook[key]; exists || len(m.addressBook) < 2048 {
			m.addressBook[key] = learnedIP{name, address, now.Add(10 * time.Minute)}
		}
	}
}

func (app *application) lanClient(ip string) bool {
	address, err := netip.ParseAddr(ip)
	if err != nil || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() {
		return false
	}
	for _, prefix := range app.lanPrefixes.Load().([]netip.Prefix) {
		if prefix.Contains(address) && address != prefix.Addr() {
			return true
		}
	}
	return false
}

func (app *application) refreshLAN() {
	prefixes := []netip.Prefix{app.lanPrefix}
	if device, err := net.InterfaceByName(app.lanDevice); err == nil {
		if addresses, err := device.Addrs(); err == nil {
			for _, addr := range addresses {
				if prefix, err := netip.ParsePrefix(addr.String()); err == nil && prefix.Addr().Is6() {
					prefixes = append(prefixes, prefix)
				}
			}
		}
	}
	app.lanPrefixes.Store(prefixes)
}

func parseConnection(line string) (connection, bool) {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return connection{}, false
	}
	item := connection{Protocol: strings.ToUpper(fields[2])}
	switch item.Protocol {
	case "TCP", "UDP", "ICMP", "ICMPV6":
	default:
		return connection{}, false
	}
	reply := false
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "src":
			if item.Client == "" {
				item.Client = value
			} else {
				reply = true
			}
		case "dst":
			if !reply {
				item.Remote = value
			}
		case "sport":
			if !reply {
				item.SourcePort = value
			}
		case "dport":
			if !reply {
				item.Port = value
			}
		case "bytes":
			count, _ := strconv.ParseUint(value, 10, 64)
			if reply {
				item.Down = count
			} else {
				item.Up = count
			}
		}
	}
	return item, item.Client != "" && item.Remote != ""
}

func deviceNames() map[string]clientDevice {
	clients := make(map[string]clientDevice)
	if contents, err := os.ReadFile("/tmp/dhcp.leases"); err == nil {
		for _, line := range strings.Split(string(contents), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				clients[fields[2]] = clientDevice{IP: fields[2], MAC: fields[1], Hostname: strings.Trim(fields[3], "*")}
			}
		}
	}
	if contents, err := os.ReadFile("/proc/net/arp"); err == nil {
		for _, line := range strings.Split(string(contents), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 6 && fields[2] != "0x0" && net.ParseIP(fields[0]) != nil {
				device := clients[fields[0]]
				device.IP, device.MAC = fields[0], fields[3]
				clients[device.IP] = device
			}
		}
	}
	return clients
}

func connectionsFromKernel() ([]connection, error) {
	file, err := os.Open("/proc/net/nf_conntrack")
	var scanner *bufio.Scanner
	if os.IsNotExist(err) {
		// MU300 kernels expose conntrack through netlink, without the procfs view.
		data, commandErr := command(1500*time.Millisecond, "conntrack", "-L", "-o", "extended")
		if commandErr != nil {
			return nil, fmt.Errorf("read conntrack through netlink: %w", commandErr)
		}
		scanner = bufio.NewScanner(bytes.NewReader(data))
	} else if err != nil {
		return nil, err
	} else {
		defer file.Close()
		scanner = bufio.NewScanner(file)
	}
	connections := make([]connection, 0, 256)
	for scanner.Scan() {
		if conn, ok := parseConnection(scanner.Text()); ok {
			connections = append(connections, conn)
		}
	}
	return connections, scanner.Err()
}

func flowKey(protocol, client, sourcePort, remote, destPort string) string {
	return strings.Join([]string{protocol, client, sourcePort, remote, destPort}, " ")
}

func bytesDelta(current, previous uint64) uint64 {
	if previous > current {
		return current
	}
	return current - previous
}

func (app *application) sampleConnections() {
	m := app.monitor
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for range tick.C {
		app.refreshLAN()
		m.Lock()
		enabled := m.enabled
		m.Unlock()
		if !enabled {
			continue
		}
		connections, err := connectionsFromKernel()
		if err != nil {
			continue
		}
		names := deviceNames()
		m.Lock()
		for key, device := range m.clients {
			device.UpRate, device.DownRate = 0, 0
			m.clients[key] = device
		}
		for key, info := range names {
			if !app.lanClient(key) || len(m.clients) >= 256 {
				continue
			}
			device := m.clients[key]
			device.IP, device.MAC, device.Hostname = key, info.MAC, info.Hostname
			m.clients[key] = device
		}
		sampledAt := time.Now()
		current := make(map[string]connection)
		for _, item := range connections {
			if !app.lanClient(item.Client) || app.lanClient(item.Remote) || len(current) >= 2048 {
				continue
			}
			key := flowKey(item.Protocol, item.Client, item.SourcePort, item.Remote, item.Port)
			entry, ok := m.captured[key]
			if !ok {
				entry = m.addressBook[item.Client+" "+item.Remote]
			}
			item.Domain = entry.Name
			previous := m.connections[key]
			up, down := bytesDelta(item.Up, previous.Up), bytesDelta(item.Down, previous.Down)
			item.lastActive = previous.lastActive
			if item.lastActive.IsZero() || up > 0 || down > 0 {
				item.lastActive = sampledAt
			}
			item.UpRate, item.DownRate = up/2, down/2
			current[key] = item
			if _, exists := m.clients[item.Client]; exists || len(m.clients) < 256 {
				device := m.clients[item.Client]
				device.IP = item.Client
				device.Up, device.Down = device.Up+up, device.Down+down
				device.UpRate, device.DownRate = device.UpRate+item.UpRate, device.DownRate+item.DownRate
				m.clients[item.Client] = device
			}
		}
		m.connections = current
		m.Unlock()
	}
}

func clientHelloName(payload []byte) string {
	if len(payload) < 44 || payload[0] != 22 || payload[5] != 1 {
		return ""
	}
	recordEnd := 5 + int(binary.BigEndian.Uint16(payload[3:5]))
	if recordEnd > len(payload) || recordEnd < 44 {
		return ""
	}
	data := payload[:recordEnd]
	offset := 44 + int(data[43])
	if offset+2 > len(data) {
		return ""
	}
	offset += 2 + int(binary.BigEndian.Uint16(data[offset:offset+2]))
	if offset >= len(data) {
		return ""
	}
	offset += 1 + int(data[offset])
	if offset+2 > len(data) {
		return ""
	}
	end := offset + 2 + int(binary.BigEndian.Uint16(data[offset:offset+2]))
	if end > len(data) {
		return ""
	}
	offset += 2
	for offset+4 <= end {
		extension := binary.BigEndian.Uint16(data[offset : offset+2])
		length := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		offset += 4
		if offset+length > end {
			return ""
		}
		if extension == 0 && length >= 5 && data[offset+2] == 0 {
			nameLength := int(binary.BigEndian.Uint16(data[offset+3 : offset+5]))
			if nameLength <= length-5 {
				return domainName(string(data[offset+5 : offset+5+nameLength]))
			}
		}
		offset += length
	}
	return ""
}

func httpHostName(payload []byte) string {
	headers, _, known := bytes.Cut(payload, []byte("\r\n\r\n"))
	if !known {
		return ""
	}
	for _, line := range bytes.Split(headers, []byte("\r\n"))[1:] {
		key, value, found := bytes.Cut(line, []byte(":"))
		if found && bytes.EqualFold(key, []byte("host")) {
			host := strings.TrimSpace(string(value))
			if name, _, err := net.SplitHostPort(host); err == nil {
				host = name
			}
			return domainName(host)
		}
	}
	return ""
}

func packetTCP(packet []byte) (client, remote, source, port string, payload []byte) {
	if len(packet) < 20 {
		return
	}
	var offset int
	switch packet[0] >> 4 {
	case 4:
		if packet[9] != 6 || binary.BigEndian.Uint16(packet[6:8])&0x1fff != 0 {
			return
		}
		offset = int(packet[0]&15) * 4
		if offset < 20 {
			return
		}
		client, remote = net.IP(packet[12:16]).String(), net.IP(packet[16:20]).String()
	case 6:
		if len(packet) < 40 || packet[6] != 6 {
			return
		}
		offset = 40
		client, remote = net.IP(packet[8:24]).String(), net.IP(packet[24:40]).String()
	default:
		return
	}
	if len(packet) < offset+20 {
		return
	}
	tcp := packet[offset:]
	start := int(tcp[12]>>4) * 4
	if start < 20 || start >= len(tcp) {
		return
	}
	source = strconv.Itoa(int(binary.BigEndian.Uint16(tcp[:2])))
	port = strconv.Itoa(int(binary.BigEndian.Uint16(tcp[2:4])))
	payload = tcp[start:]
	return
}

func (app *application) captureNames() error {
	device, err := net.InterfaceByName(app.lanDevice)
	if err != nil {
		return err
	}
	protocol := int(unix.ETH_P_ALL << 8)
	socket, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, protocol)
	if err != nil {
		return err
	}
	defer unix.Close(socket)
	if err := unix.Bind(socket, &unix.SockaddrLinklayer{Protocol: uint16(protocol), Ifindex: device.Index}); err != nil {
		return err
	}
	m := app.monitor
	m.Lock()
	m.packetCapture = true
	m.Unlock()
	defer func() {
		m.Lock()
		m.packetCapture = false
		m.Unlock()
	}()
	packet := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(socket, packet, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		client, remote, source, port, payload := packetTCP(packet[:n])
		if len(payload) < 5 || !app.lanClient(client) || app.lanClient(remote) {
			continue
		}
		name := clientHelloName(payload)
		if name == "" && payload[0] >= 'A' && payload[0] <= 'Z' {
			name = httpHostName(payload)
		}
		if !validDomain(name, false) {
			continue
		}
		key := flowKey("TCP", client, source, remote, port)
		m.Lock()
		if _, exists := m.captured[key]; m.enabled && (exists || len(m.captured) < 2048) {
			m.captured[key] = learnedIP{name, remote, time.Now().Add(10 * time.Minute)}
		}
		m.Unlock()
	}
}
