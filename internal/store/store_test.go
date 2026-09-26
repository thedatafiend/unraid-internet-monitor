package store

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

func f(v float64) *float64 { return &v }

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateIsIdempotentAndIncremental(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var av int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&av); err != nil || av != 2 {
		t.Fatalf("auto_vacuum = %d (%v), want 2 (incremental)", av, err)
	}
	s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s.Close()
}

func TestTargets(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	gw := model.Target{Key: "icmp:gateway:ip4", Kind: "icmp", Role: model.RoleGateway, Name: "Gateway",
		Addr: netip.MustParseAddr("192.168.1.1"), Family: model.FamilyV4}
	id1, err := s.UpsertTarget(ctx, gw)
	if err != nil {
		t.Fatal(err)
	}
	// Same key, new address (router replaced): same ID, address updated.
	gw.Addr = netip.MustParseAddr("192.168.1.254")
	id2, err := s.UpsertTarget(ctx, gw)
	if err != nil || id2 != id1 {
		t.Fatalf("upsert by key gave id %d (err %v), want %d", id2, err, id1)
	}
	cf := model.Target{Key: "icmp:1.1.1.1", Kind: "icmp", Role: model.RoleInternet, Name: "Cloudflare",
		Addr: netip.MustParseAddr("1.1.1.1"), Family: model.FamilyV4}
	id3, err := s.UpsertTarget(ctx, cf)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DisableTargetsExcept(ctx, []int64{id3}); err != nil {
		t.Fatal(err)
	}
	ts, err := s.Targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].Enabled || !ts[1].Enabled || ts[0].Addr.String() != "192.168.1.254" {
		t.Fatalf("targets = %+v", ts)
	}
}

func TestWriteMinutesMergesPartialMinute(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	// First half of a minute, flushed at shutdown...
	first := model.MinuteStat{TargetID: 1, TS: 600, Sent: 30, Recv: 30,
		Min: f(10), Avg: f(10), P50: f(10), P95: f(12), P99: f(12), Max: f(12), Jitter: f(1)}
	// ...and the second half after restart, including a dead period.
	second := model.MinuteStat{TargetID: 1, TS: 600, Sent: 30, Recv: 10,
		Min: f(8), Avg: f(20), P50: f(20), P95: f(30), P99: f(40), Max: f(40), Jitter: f(5)}
	if err := s.WriteMinutes(ctx, []model.MinuteStat{first}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteMinutes(ctx, []model.MinuteStat{second}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Series(ctx, 1, 0, 1000, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d buckets", len(got))
	}
	b := got[0]
	if b.Sent != 60 || b.Recv != 40 || *b.Min != 8 || *b.Max != 40 || *b.P95 != 30 || *b.P99 != 40 {
		t.Fatalf("merged bucket = %+v", b)
	}
	if want := (10.0*30 + 20*10) / 40; *b.Avg != want {
		t.Fatalf("avg = %v, want %v", *b.Avg, want)
	}
	if want := (1.0*30 + 5*10) / 40; *b.Jitter != want {
		t.Fatalf("jitter = %v, want %v", *b.Jitter, want)
	}
}

func TestSeriesDownsampleAndPrune(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	var stats []model.MinuteStat
	for i := int64(0); i < 10; i++ {
		m := model.MinuteStat{TargetID: 1, TS: i * 60, Sent: 60, Recv: 60,
			Min: f(10), Avg: f(float64(10 + i)), P50: f(10), P95: f(float64(20 + i)), P99: f(30), Max: f(30), Jitter: f(1)}
		if i == 4 { // an outage minute: nothing received
			m.Recv = 0
			m.Min, m.Avg, m.P50, m.P95, m.P99, m.Max, m.Jitter = nil, nil, nil, nil, nil, nil, nil
		}
		stats = append(stats, m)
	}
	stats = append(stats, model.MinuteStat{TargetID: 2, TS: 0, Sent: 60, Recv: 60, Avg: f(99)})
	if err := s.WriteMinutes(ctx, stats); err != nil {
		t.Fatal(err)
	}

	got, err := s.Series(ctx, 1, 0, 600, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 five-minute buckets, got %+v", got)
	}
	a := got[0] // minutes 0..4, minute 4 empty
	if a.TS != 0 || a.Sent != 300 || a.Recv != 240 {
		t.Fatalf("bucket 0 counts = %+v", a)
	}
	if want := (10.0 + 11 + 12 + 13) / 4; *a.Avg != want {
		t.Fatalf("bucket 0 avg = %v, want %v (null minute must not dilute)", *a.Avg, want)
	}
	if *a.P95 != 23 {
		t.Fatalf("bucket 0 p95 = %v, want worst minute 23", *a.P95)
	}

	n, err := s.Prune(ctx, 300)
	if err != nil || n != 6 { // 5 rows of target 1 + 1 row of target 2
		t.Fatalf("pruned %d (err %v), want 6", n, err)
	}
	got, _ = s.Series(ctx, 1, 0, 600, 60)
	if len(got) != 5 || got[0].TS != 300 {
		t.Fatalf("after prune: %+v", got)
	}
	if size, err := s.SizeBytes(ctx); err != nil || size <= 0 {
		t.Fatalf("size = %d, %v", size, err)
	}
}
