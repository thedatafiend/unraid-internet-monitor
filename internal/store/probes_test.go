package store

import (
	"context"
	"testing"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

func TestHTTPTargetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	url := "https://www.google.com/generate_204"
	id, err := s.UpsertTarget(ctx, model.Target{Key: "http:" + url, Kind: model.KindHTTP, Role: model.RoleHTTP, Name: "Google", URL: url})
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := s.Targets(ctx)
	if len(ts) != 1 || ts[0].ID != id || ts[0].URL != url || ts[0].Addr.IsValid() {
		t.Fatalf("targets = %+v", ts)
	}
}

func TestHTTPSamples(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	err := s.WriteHTTPSamples(ctx, []model.HTTPSample{
		{TargetID: 1, TS: 100, OK: true, Status: 204, DNSMs: f(3), ConnectMs: f(10), TLSMs: f(20), TTFBMs: f(30), TotalMs: f(65)},
		{TargetID: 1, TS: 160, OK: false, Error: "timeout", TotalMs: f(10000)},
		{TargetID: 2, TS: 100, OK: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.HTTPSamples(ctx, 1, 0, 1000)
	if err != nil || len(got) != 2 || *got[0].TLSMs != 20 || got[1].OK || got[1].DNSMs != nil || got[1].Error != "timeout" {
		t.Fatalf("samples = %+v, %v", got, err)
	}
}

func TestPublicIPHistory(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if p, err := s.LatestPublicIP(ctx); err != nil || p.TS != 0 {
		t.Fatalf("empty: %+v %v", p, err)
	}
	s.RecordPublicIP(ctx, model.PublicIP{TS: 100, IPv4: "203.0.113.1"})
	s.RecordPublicIP(ctx, model.PublicIP{TS: 200, IPv4: "203.0.113.2"})
	if p, _ := s.LatestPublicIP(ctx); p.IPv4 != "203.0.113.2" {
		t.Fatalf("latest = %+v", p)
	}
	// Retention keeps the newest row even when it is older than the cutoff.
	if _, err := s.Prune(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	all, _ := s.PublicIPs(ctx, 0, 2000)
	if len(all) != 1 || all[0].IPv4 != "203.0.113.2" {
		t.Fatalf("after prune = %+v", all)
	}
}

func TestTraces(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ev := int64(7)
	a := &model.Trace{TS: 100, Reason: "outage", EventID: &ev, Dst: "1.1.1.1",
		Hops: []model.Hop{{TTL: 1, Addr: "192.168.1.1", RTTMs: 0.9}, {TTL: 2}}}
	b := &model.Trace{TS: 200, Reason: "discovery", Dst: "1.1.1.1", Hops: []model.Hop{{TTL: 1, Addr: "1.1.1.1", Reached: true}}}
	if err := s.InsertTrace(ctx, a); err != nil {
		t.Fatal(err)
	}
	s.InsertTrace(ctx, b)
	byEvent, _ := s.Traces(ctx, 7, 0, 0, 10)
	if len(byEvent) != 1 || byEvent[0].ID != a.ID || len(byEvent[0].Hops) != 2 || byEvent[0].Hops[1].Addr != "" {
		t.Fatalf("by event = %+v", byEvent)
	}
	recent, _ := s.Traces(ctx, 0, 0, 1000, 10)
	if len(recent) != 2 || recent[0].ID != b.ID || recent[0].EventID != nil {
		t.Fatalf("recent = %+v", recent)
	}
}
