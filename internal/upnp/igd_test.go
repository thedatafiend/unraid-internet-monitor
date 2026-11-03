package upnp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRouter mimics a TP-Link Deco X55 running MiniUPnPd 1.8: it reports
// link status, counters and the WAN address, but rejects GetStatusInfo.
type fakeRouter struct {
	srv *httptest.Server

	mu    sync.Mutex
	link  string
	extIP string
	rx    uint64
	tx    uint64
	calls map[string]int
}

const deviceDesc = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
<specVersion><major>1</major><minor>1</minor></specVersion>
<device>
 <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
 <friendlyName>Deco X55</friendlyName><manufacturer>TP-LINK</manufacturer><modelName>X55</modelName>
 <serviceList><service><serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType>
  <controlURL>/yhrjw/ctl/L3F</controlURL></service></serviceList>
 <deviceList><device>
  <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
  <manufacturer>MiniUPnP</manufacturer><modelName>WAN Device</modelName>
  <serviceList><service><serviceType>urn:schemas-upnp-org:service:WANCommonInterfaceConfig:1</serviceType>
   <controlURL>/yhrjw/ctl/CmnIfCfg</controlURL></service></serviceList>
  <deviceList><device>
   <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
   <manufacturer>MiniUPnP</manufacturer><modelName>MiniUPnPd</modelName>
   <serviceList><service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
    <controlURL>/yhrjw/ctl/IPConn</controlURL></service></serviceList>
  </device></deviceList>
 </device></deviceList>
</device>
</root>`

func newFakeRouter(t *testing.T) *fakeRouter {
	f := &fakeRouter{link: "Up", extIP: "207.204.59.53", rx: 9709807900, tx: 1234567, calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRouter) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/yhrjw/rootDesc.xml" {
		io.WriteString(w, deviceDesc)
		return
	}
	io.Copy(io.Discard, r.Body)
	_, action, _ := strings.Cut(strings.Trim(r.Header.Get("SOAPAction"), `"`), "#")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[action]++
	reply := func(fields string) {
		fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+
			`<u:%sResponse xmlns:u="x">%s</u:%sResponse></s:Body></s:Envelope>`, action, fields, action)
	}
	switch action {
	case "GetCommonLinkProperties":
		reply(`<NewWANAccessType>Cable</NewWANAccessType><NewLayer1UpstreamMaxBitRate>4194304</NewLayer1UpstreamMaxBitRate>` +
			`<NewLayer1DownstreamMaxBitRate>8388608</NewLayer1DownstreamMaxBitRate><NewPhysicalLinkStatus>` + f.link + `</NewPhysicalLinkStatus>`)
	case "GetTotalBytesReceived":
		reply(fmt.Sprintf(`<NewTotalBytesReceived>%d</NewTotalBytesReceived>`, f.rx))
	case "GetTotalBytesSent":
		reply(fmt.Sprintf(`<NewTotalBytesSent>%d</NewTotalBytesSent>`, f.tx))
	case "GetExternalIPAddress":
		reply(`<NewExternalIPAddress>` + f.extIP + `</NewExternalIPAddress>`)
	default:
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>`+
			`<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">`+
			`<errorCode>401</errorCode><errorDescription>Invalid Action</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
	}
}

// ssdpResponder answers M-SEARCH requests on a local UDP port with location.
func ssdpResponder(t *testing.T, location string) int {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if !strings.Contains(string(buf[:n]), "M-SEARCH") {
				continue
			}
			conn.WriteToUDP([]byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\n"+
				"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n"+
				"USN: uuid:1-2-3::urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\nEXT:\r\n"+
				"SERVER: TP-LINK/TP-LINK UPnP/1.1 MiniUPnPd/1.8\r\nLOCATION: "+location+"\r\n\r\n"), from)
		}
	}()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

var localhost = netip.MustParseAddr("127.0.0.1")

func TestDiscoverAndStatus(t *testing.T) {
	f := newFakeRouter(t)
	port := ssdpResponder(t, f.srv.URL+"/yhrjw/rootDesc.xml")
	ctx := context.Background()
	d, err := Discover(ctx, localhost, Options{Port: port, NoMulticast: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if d.Manufacturer != "TP-LINK" || d.Model != "X55" || d.Server != "TP-LINK/TP-LINK UPnP/1.1 MiniUPnPd/1.8" || d.Addr != localhost {
		t.Fatalf("device = %+v", d)
	}
	if !strings.HasSuffix(d.commonURL, "/yhrjw/ctl/CmnIfCfg") || !strings.HasSuffix(d.connURL, "/yhrjw/ctl/IPConn") {
		t.Fatalf("control URLs = %q %q", d.commonURL, d.connURL)
	}

	s, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.LinkStatus != "Up" || s.ExternalIP != "207.204.59.53" || s.ConnStatus != "" || s.UptimeS != nil {
		t.Fatalf("status = %+v", s)
	}
	if s.RxBytes == nil || *s.RxBytes != 9709807900 || s.TxBytes == nil || *s.TxBytes != 1234567 {
		t.Fatalf("counters = %v %v", s.RxBytes, s.TxBytes)
	}
	if up := s.Up(); up == nil || !*up {
		t.Fatalf("Up() = %v", up)
	}

	// GetStatusInfo was rejected as an invalid action, so it is not asked again.
	f.mu.Lock()
	f.link, f.extIP = "Down", "0.0.0.0"
	f.mu.Unlock()
	s, err = d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if up := s.Up(); up == nil || *up {
		t.Fatalf("Up() after the WAN went down = %v (%+v)", up, s)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls["GetStatusInfo"] != 1 || f.calls["GetCommonLinkProperties"] != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestDiscoverRejectsLocationOffTheGateway(t *testing.T) {
	port := ssdpResponder(t, "http://192.0.2.10:1900/rootDesc.xml")
	_, err := Discover(context.Background(), localhost, Options{Port: port, NoMulticast: true, Timeout: 2 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "not on the gateway") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiscoverNoReply(t *testing.T) {
	// Nothing listens on this port.
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	start := time.Now()
	_, err = Discover(context.Background(), localhost, Options{Port: port, NoMulticast: true, Timeout: 300 * time.Millisecond})
	if err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

func TestStatusUnreachable(t *testing.T) {
	f := newFakeRouter(t)
	d, err := Open(context.Background(), f.srv.URL+"/yhrjw/rootDesc.xml")
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Close()
	if _, err := d.Status(context.Background()); err == nil {
		t.Fatal("expected an error from a router that is gone")
	}
}

func TestOpenResolvesURLBaseAndPPP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="ISO-8859-1"?><root><URLBase>http://%s/upnp/</URLBase><device>
			<friendlyName>Old Router</friendlyName><deviceList><device><deviceList><device><serviceList>
			<service><serviceType>urn:schemas-upnp-org:service:WANPPPConnection:1</serviceType><controlURL>ppp</controlURL></service>
			</serviceList></device></deviceList></device></deviceList></device></root>`, r.Host)
	}))
	defer srv.Close()
	d, err := Open(context.Background(), srv.URL+"/desc.xml")
	if err != nil {
		t.Fatal(err)
	}
	if d.connURL != srv.URL+"/upnp/ppp" || !strings.HasPrefix(d.connType, svcPPP) || d.commonURL != "" || d.Model != "Old Router" {
		t.Fatalf("device = %+v", d)
	}
}

func TestOpenNeedsWANServices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<root><device><friendlyName>Printer</friendlyName></device></root>`)
	}))
	defer srv.Close()
	if _, err := Open(context.Background(), srv.URL); err == nil {
		t.Fatal("expected an error for a device without WAN services")
	}
}

func TestStatusUp(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		s    Status
		want *bool
	}{
		{Status{}, nil},
		{Status{ExternalIP: "203.0.113.5"}, nil}, // an address alone says nothing about the link
		{Status{LinkStatus: "Up"}, &yes},
		{Status{LinkStatus: "Down"}, &no},
		{Status{LinkStatus: "Initializing"}, &no},
		{Status{LinkStatus: "Up", ExternalIP: "0.0.0.0"}, &no},
		{Status{LinkStatus: "Up", ConnStatus: "Disconnected"}, &no},
		{Status{ConnStatus: "Connected"}, &yes},
	}
	for _, tt := range tests {
		got := tt.s.Up()
		if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
			t.Errorf("%+v.Up() = %v, want %v", tt.s, got, tt.want)
		}
	}
}
