package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/alert"
	"github.com/thedatafiend/unraid-internet-monitor/internal/config"
	"github.com/thedatafiend/unraid-internet-monitor/internal/store"
)

// fakeIGD is a minimal UPnP router: link status and a received-bytes counter.
type fakeIGD struct {
	mu   sync.Mutex
	link string
	rx   uint64
}

func (f *fakeIGD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		io.WriteString(w, `<root><device><manufacturer>TP-LINK</manufacturer><modelName>X55</modelName><serviceList><service>
			<serviceType>urn:schemas-upnp-org:service:WANCommonInterfaceConfig:1</serviceType><controlURL>/ctl/CmnIfCfg</controlURL>
			</service></serviceList></device></root>`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch action := r.Header.Get("SOAPAction"); {
	case strings.Contains(action, "GetCommonLinkProperties"):
		fmt.Fprintf(w, `<Envelope><Body><R><NewPhysicalLinkStatus>%s</NewPhysicalLinkStatus></R></Body></Envelope>`, f.link)
	case strings.Contains(action, "GetTotalBytesReceived"):
		fmt.Fprintf(w, `<Envelope><Body><R><NewTotalBytesReceived>%d</NewTotalBytesReceived></R></Body></Envelope>`, f.rx)
	default:
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `<Envelope><Body><Fault><detail><UPnPError><errorCode>401</errorCode></UPnPError></detail></Fault></Body></Envelope>`)
	}
}

func (f *fakeIGD) set(link string, rx uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.link, f.rx = link, rx
}

func TestRouterReadings(t *testing.T) {
	ctx := context.Background()
	f := &fakeIGD{link: "Up", rx: 1000}
	srv := httptest.NewServer(f)
	defer srv.Close()

	cfg := config.Defaults()
	cfg.RouterUPnP = srv.URL + "/rootDesc.xml"
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := New(cfg, nil, nil, st, alert.NewManager(alert.Config{}, nil, st, log), log)

	if w := e.wanObservation(100); w.Enabled || w.Known {
		t.Fatalf("before any reading: %+v", w)
	}
	t0 := time.Unix(1000, 0)
	e.routerRound(ctx, t0)
	rs := e.RouterStatus()
	if rs.Mode != "url" || !rs.Found || rs.Model != "X55" || rs.LinkStatus != "Up" || rs.WANUp == nil || !*rs.WANUp || rs.RxBps != nil {
		t.Fatalf("first reading: %+v", rs)
	}

	f.set("Up", 6000)
	e.routerRound(ctx, t0.Add(5*time.Second))
	if rs := e.RouterStatus(); rs.RxBps == nil || *rs.RxBps != 8000 || rs.TxBps != nil {
		t.Fatalf("rate: %+v", rs)
	}
	if w := e.wanObservation(1006); !w.Enabled || !w.Known || !w.Up || w.At != 1005 || !w.HasRx || w.RxBytes != 6000 {
		t.Fatalf("observation: %+v", w)
	}
	if w := e.wanObservation(1100); !w.Enabled || w.Known || w.HasRx {
		t.Fatalf("a stale reading must not count: %+v", w)
	}

	f.set("Down", 6000)
	e.routerRound(ctx, t0.Add(10*time.Second))
	if w := e.wanObservation(1010); !w.Known || w.Up {
		t.Fatalf("WAN down: %+v", w)
	}

	// The router goes away: after three failed readings it is looked up
	// again, and a failed lookup is retried later rather than every round.
	srv.Close()
	for i := 3; i < 6; i++ {
		e.routerRound(ctx, t0.Add(time.Duration(5*i)*time.Second))
	}
	if rs := e.RouterStatus(); rs.Found || rs.Error == "" {
		t.Fatalf("after failures: %+v", rs)
	}
	e.routerRound(ctx, t0.Add(30*time.Second)) // lookup fails
	e.rt.mu.Lock()
	next := e.rt.nextFind
	e.rt.mu.Unlock()
	if want := t0.Add(30*time.Second + routerRetryLost); !next.Equal(want) {
		t.Fatalf("next lookup at %v, want %v", next, want)
	}
}
