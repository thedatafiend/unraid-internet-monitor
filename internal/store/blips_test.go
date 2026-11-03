package store

import (
	"context"
	"net/netip"
	"testing"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

func TestBlipCountsAndPrune(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.InsertBlips(ctx, []model.Blip{
		{TS: 100, Seconds: 1, Class: model.ClassISPEdge},
		{TS: 200, Seconds: 2, Class: model.ClassISPEdge},
		{TS: 250, Seconds: 1, Class: model.ClassLocal},
		{TS: 700, Seconds: 2, Class: model.ClassISPEdge},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.BlipCounts(ctx, 0, 1000, 600)
	if err != nil {
		t.Fatal(err)
	}
	want := []BlipCount{
		{TS: 0, Class: model.ClassISPEdge, Count: 2, Seconds: 3},
		{TS: 0, Class: model.ClassLocal, Count: 1, Seconds: 1},
		{TS: 600, Class: model.ClassISPEdge, Count: 1, Seconds: 2},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("bucket %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := s.Prune(ctx, 300); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.BlipCounts(ctx, 0, 1000, 60); len(got) != 1 || got[0].TS != 660 {
		t.Fatalf("after prune: %+v", got)
	}
}

func TestInternetStatsSkipsOutageMinutes(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	inet, err := s.UpsertTarget(ctx, model.Target{Key: "icmp:1.1.1.1", Kind: model.KindICMP, Role: model.RoleInternet,
		Name: "Cloudflare", Addr: netip.MustParseAddr("1.1.1.1"), Family: model.FamilyV4})
	if err != nil {
		t.Fatal(err)
	}
	gw, err := s.UpsertTarget(ctx, model.Target{Key: "icmp:gateway:ip4", Kind: model.KindICMP, Role: model.RoleGateway,
		Name: "Gateway", Addr: netip.MustParseAddr("192.168.68.1"), Family: model.FamilyV4})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteMinutes(ctx, []model.MinuteStat{
		{TargetID: inet, TS: 0, Sent: 60, Recv: 59, Avg: f(10)},
		{TargetID: inet, TS: 60, Sent: 60, Recv: 0},               // outage minute
		{TargetID: inet, TS: 120, Sent: 60, Recv: 30, Avg: f(40)}, // outage still open at its start
		{TargetID: inet, TS: 180, Sent: 60, Recv: 60, Avg: f(20)},
		{TargetID: gw, TS: 0, Sent: 60, Recv: 0}, // not an internet target
	}); err != nil {
		t.Fatal(err)
	}
	end := int64(125)
	if err := s.InsertEvent(ctx, &model.Event{Kind: model.EventOutage, Scope: model.FamilyV4, StartedAt: 70, EndedAt: &end}); err != nil {
		t.Fatal(err)
	}
	st, err := s.InternetStats(ctx, 0, 240, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if st.Sent != 120 || st.Recv != 119 || st.Avg == nil || *st.Avg < 14.9 || *st.Avg > 15.1 {
		t.Fatalf("stats = %+v avg %v", st, *st.Avg)
	}
	if st, err := s.InternetStats(ctx, 1000, 2000, 2000); err != nil || st.Sent != 0 || st.Avg != nil {
		t.Fatalf("empty window = %+v, %v", st, err)
	}
}

func TestMarkers(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	a := model.Marker{TS: 2000, Label: "ONT replaced"}
	b := model.Marker{TS: 1000, Label: "Router moved"}
	for _, m := range []*model.Marker{&a, &b} {
		if err := s.InsertMarker(ctx, m); err != nil || m.ID == 0 {
			t.Fatalf("insert: %v %+v", err, m)
		}
	}
	ms, err := s.Markers(ctx)
	if err != nil || len(ms) != 2 || ms[0] != b || ms[1] != a {
		t.Fatalf("markers = %+v, %v", ms, err)
	}
	if ok, err := s.DeleteMarker(ctx, b.ID); !ok || err != nil {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, _ := s.DeleteMarker(ctx, b.ID); ok {
		t.Fatal("deleted twice")
	}
	if _, err := s.Prune(ctx, 1<<40); err != nil {
		t.Fatal(err)
	}
	if ms, _ := s.Markers(ctx); len(ms) != 1 {
		t.Fatalf("markers must survive retention: %+v", ms)
	}
}
