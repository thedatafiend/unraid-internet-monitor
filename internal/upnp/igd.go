// Package upnp reads the router's own view of its internet connection over
// UPnP IGD: whether its WAN port is up, its WAN address and its traffic
// counters. It only reads; it never adds port mappings or changes anything.
package upnp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	ssdpGroup    = "239.255.255.250"
	searchTarget = "urn:schemas-upnp-org:device:InternetGatewayDevice:1"
	maxBody      = 64 << 10
	callTimeout  = 3 * time.Second
)

// Service types, matched without their version suffix.
const (
	svcCommon = "urn:schemas-upnp-org:service:WANCommonInterfaceConfig:"
	svcIPConn = "urn:schemas-upnp-org:service:WANIPConnection:"
	svcPPP    = "urn:schemas-upnp-org:service:WANPPPConnection:"
)

// Options tune Discover.
type Options struct {
	Iface       string        // interface for the multicast search; "" lets the kernel choose
	Port        int           // SSDP port, 1900 when 0
	Timeout     time.Duration // how long to wait for a reply, 3 s when 0
	NoMulticast bool          // ask the gateway directly only
}

// Device is a router found over UPnP.
type Device struct {
	Location     string     `json:"location"`
	Addr         netip.Addr `json:"address"`
	Manufacturer string     `json:"manufacturer,omitempty"`
	Model        string     `json:"model,omitempty"`
	Server       string     `json:"server,omitempty"` // SSDP SERVER header, e.g. "MiniUPnPd/1.8"

	commonURL, commonType string // WANCommonInterfaceConfig
	connURL, connType     string // WANIPConnection or WANPPPConnection

	client *http.Client

	mu          sync.Mutex
	unsupported map[string]bool // actions the router rejected as not implemented
}

// Status is one reading of the router's WAN state. Anything the router does
// not report is left empty.
type Status struct {
	LinkStatus string  // Up, Down, Initializing or Unavailable
	ConnStatus string  // Connected, Disconnected, ... (GetStatusInfo; often not implemented)
	UptimeS    *int64  // seconds the WAN connection has been up, with ConnStatus
	ExternalIP string  // the router's WAN address
	RxBytes    *uint64 // total bytes received on the WAN port
	TxBytes    *uint64
}

// Up reports whether the router says its WAN connection is up, or nil when
// it gave no indication. Any sign of "down" wins.
func (s Status) Up() *bool {
	known, up := false, true
	if s.LinkStatus != "" {
		known, up = true, up && s.LinkStatus == "Up"
	}
	if s.ConnStatus != "" {
		known, up = true, up && s.ConnStatus == "Connected"
	}
	if s.ExternalIP == "0.0.0.0" { // what MiniUPnPd reports when the WAN has no address
		known, up = true, false
	}
	if !known {
		return nil
	}
	return &up
}

// SOAPError is a UPnP fault returned by the router.
type SOAPError struct {
	Code int
	Desc string
}

func (e *SOAPError) Error() string { return fmt.Sprintf("UPnP error %d %s", e.Code, e.Desc) }

// notImplemented reports whether a fault means the router does not support
// the action at all (so there is no point asking again).
func (e *SOAPError) notImplemented() bool { return e.Code == 401 || e.Code == 602 }

// Discover finds the UPnP internet gateway device on gateway. The search is
// sent to the gateway directly and to the multicast group, and only a reply
// from the gateway itself is accepted.
func Discover(ctx context.Context, gateway netip.Addr, opt Options) (*Device, error) {
	loc, server, err := search(ctx, gateway, opt)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(loc)
	if err != nil {
		return nil, fmt.Errorf("bad LOCATION %q: %w", loc, err)
	}
	// The description must come from the gateway that answered, not
	// wherever a reply claims it is.
	if host, err := netip.ParseAddr(u.Hostname()); err != nil || host.Unmap() != gateway {
		return nil, fmt.Errorf("LOCATION %q is not on the gateway %s", loc, gateway)
	}
	d, err := Open(ctx, loc)
	if err != nil {
		return nil, err
	}
	d.Server = server
	return d, nil
}

func search(ctx context.Context, gateway netip.Addr, opt Options) (location, server string, err error) {
	port := opt.Port
	if port == 0 {
		port = 1900
	}
	timeout := opt.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetReadDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Now()) })
	defer stop()

	msg := func(host string) []byte {
		return []byte("M-SEARCH * HTTP/1.1\r\nHOST: " + host + "\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " +
			searchTarget + "\r\nUSER-AGENT: internet-monitor UPnP/1.1\r\n\r\n")
	}
	direct := net.UDPAddrFromAddrPort(netip.AddrPortFrom(gateway, uint16(port)))
	group := &net.UDPAddr{IP: net.ParseIP(ssdpGroup), Port: 1900}
	if !opt.NoMulticast {
		pc := ipv4.NewPacketConn(conn)
		if opt.Iface != "" {
			if ifi, err := net.InterfaceByName(opt.Iface); err == nil {
				pc.SetMulticastInterface(ifi)
			}
		}
		pc.SetMulticastTTL(2)
	}
	// UDP can be lost; send each search twice.
	var sendErr error
	for range 2 {
		if _, err := conn.WriteToUDP(msg(direct.String()), direct); err != nil {
			sendErr = err
		}
		if !opt.NoMulticast {
			conn.WriteToUDP(msg(group.String()), group) // best effort: the direct search is the one that matters
		}
	}

	buf := make([]byte, 4096)
	var others []string
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			break // deadline or cancelled
		}
		if from.Addr().Unmap() != gateway {
			others = append(others, from.Addr().String())
			continue
		}
		loc, srv, ok := parseSearchReply(buf[:n])
		if ok {
			return loc, srv, nil
		}
	}
	if ctx.Err() != nil {
		return "", "", ctx.Err()
	}
	switch {
	case sendErr != nil:
		return "", "", fmt.Errorf("sending UPnP search to %s: %w", gateway, sendErr)
	case len(others) > 0:
		return "", "", fmt.Errorf("no UPnP internet gateway reply from %s (other devices replied: %s)", gateway, strings.Join(others, ", "))
	}
	return "", "", fmt.Errorf("no UPnP internet gateway reply from %s; is UPnP turned on in the router?", gateway)
}

// parseSearchReply reads an SSDP search response and returns its LOCATION
// when it describes an internet gateway.
func parseSearchReply(b []byte) (location, server string, ok bool) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(b)), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", "", false
	}
	resp.Body.Close()
	st := resp.Header.Get("St") + " " + resp.Header.Get("Usn")
	loc := resp.Header.Get("Location")
	if loc == "" || !strings.Contains(st, "InternetGatewayDevice") {
		return "", "", false
	}
	return loc, resp.Header.Get("Server"), true
}

// Open reads the device description at location and finds its WAN services.
func Open(ctx context.Context, location string) (*Device, error) {
	base, err := url.Parse(location)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("bad device description URL %q", location)
	}
	d := &Device{
		Location:    location,
		client:      &http.Client{Timeout: callTimeout, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}},
		unsupported: map[string]bool{},
	}
	d.Addr, _ = netip.ParseAddr(base.Hostname())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading device description: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading device description: HTTP %d", resp.StatusCode)
	}
	var root struct {
		URLBase string    `xml:"URLBase"`
		Device  xmlDevice `xml:"device"`
	}
	dec := xml.NewDecoder(io.LimitReader(resp.Body, maxBody))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("parsing device description: %w", err)
	}
	if root.URLBase != "" {
		if b, err := url.Parse(root.URLBase); err == nil {
			base = b
		}
	}
	d.Manufacturer = strings.TrimSpace(root.Device.Manufacturer)
	d.Model = strings.TrimSpace(root.Device.ModelName)
	if d.Model == "" {
		d.Model = strings.TrimSpace(root.Device.FriendlyName)
	}

	var walk func(dev xmlDevice)
	walk = func(dev xmlDevice) {
		for _, s := range dev.Services {
			ctl, err := url.Parse(strings.TrimSpace(s.ControlURL))
			if err != nil {
				continue
			}
			abs := base.ResolveReference(ctl).String()
			st := strings.TrimSpace(s.ServiceType)
			switch {
			case strings.HasPrefix(st, svcCommon) && d.commonURL == "":
				d.commonURL, d.commonType = abs, st
			case strings.HasPrefix(st, svcIPConn) && (d.connURL == "" || strings.HasPrefix(d.connType, svcPPP)):
				d.connURL, d.connType = abs, st // prefer IP over PPP
			case strings.HasPrefix(st, svcPPP) && d.connURL == "":
				d.connURL, d.connType = abs, st
			}
		}
		for _, c := range dev.Devices {
			walk(c)
		}
	}
	walk(root.Device)
	if d.commonURL == "" && d.connURL == "" {
		return nil, errors.New("the router's UPnP description has no WAN status services")
	}
	return d, nil
}

type xmlDevice struct {
	FriendlyName string       `xml:"friendlyName"`
	Manufacturer string       `xml:"manufacturer"`
	ModelName    string       `xml:"modelName"`
	Services     []xmlService `xml:"serviceList>service"`
	Devices      []xmlDevice  `xml:"deviceList>device"`
}

type xmlService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

// Status reads the router's current WAN state. Actions the router does not
// implement are skipped (and not asked again); the reading fails only when
// the router could not be reached or answered nothing useful.
func (d *Device) Status(ctx context.Context) (Status, error) {
	var s Status
	got := false
	read := func(ctlURL, svc, action string, use func(map[string]string)) error {
		if ctlURL == "" || d.isUnsupported(action) {
			return nil
		}
		out, err := d.call(ctx, ctlURL, svc, action)
		var fault *SOAPError
		switch {
		case errors.As(err, &fault):
			if fault.notImplemented() {
				d.markUnsupported(action)
			}
			return nil // the router is there; it just has no answer to this one
		case err != nil:
			return err
		}
		got = true
		use(out)
		return nil
	}
	counter := func(v string) *uint64 {
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	steps := []func() error{
		func() error {
			return read(d.commonURL, d.commonType, "GetCommonLinkProperties", func(m map[string]string) {
				s.LinkStatus = m["NewPhysicalLinkStatus"]
			})
		},
		func() error {
			return read(d.connURL, d.connType, "GetStatusInfo", func(m map[string]string) {
				s.ConnStatus = m["NewConnectionStatus"]
				if v, err := strconv.ParseInt(m["NewUptime"], 10, 64); err == nil {
					s.UptimeS = &v
				}
			})
		},
		func() error {
			return read(d.connURL, d.connType, "GetExternalIPAddress", func(m map[string]string) {
				s.ExternalIP = m["NewExternalIPAddress"]
			})
		},
		func() error {
			return read(d.commonURL, d.commonType, "GetTotalBytesReceived", func(m map[string]string) {
				s.RxBytes = counter(m["NewTotalBytesReceived"])
			})
		},
		func() error {
			return read(d.commonURL, d.commonType, "GetTotalBytesSent", func(m map[string]string) {
				s.TxBytes = counter(m["NewTotalBytesSent"])
			})
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return Status{}, err
		}
	}
	if !got {
		return Status{}, errors.New("the router answered no WAN status request")
	}
	return s, nil
}

func (d *Device) isUnsupported(action string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.unsupported[action]
}

func (d *Device) markUnsupported(action string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unsupported[action] = true
}

// call invokes a SOAP action without arguments and returns the response's
// leaf elements by local name.
func (d *Device) call(ctx context.Context, ctlURL, serviceType, action string) (map[string]string, error) {
	body := `<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:` + action + ` xmlns:u="` + serviceType + `"></u:` + action + `></s:Body></s:Envelope>`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ctlURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+serviceType+"#"+action+`"`)
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, perr := leaves(io.LimitReader(resp.Body, maxBody))
	switch {
	case resp.StatusCode == http.StatusOK && perr == nil:
		return out, nil
	case resp.StatusCode == http.StatusInternalServerError && out["errorCode"] != "":
		code, _ := strconv.Atoi(out["errorCode"])
		return nil, &SOAPError{Code: code, Desc: out["errorDescription"]}
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: HTTP %d", action, resp.StatusCode)
	}
	return nil, fmt.Errorf("%s: %w", action, perr)
}

// leaves returns the text of every element that has no child elements.
func leaves(r io.Reader) (map[string]string, error) {
	dec := xml.NewDecoder(r)
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	out := map[string]string{}
	var cur string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			cur = t.Name.Local
			text.Reset()
		case xml.CharData:
			if cur != "" {
				text.Write(t)
			}
		case xml.EndElement:
			if cur == t.Name.Local {
				out[cur] = strings.TrimSpace(text.String())
			}
			cur = ""
		}
	}
}
