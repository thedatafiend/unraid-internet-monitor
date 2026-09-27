package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"

	_ "modernc.org/sqlite"
)

func i64(v int64) *int64 { return &v }

func TestEventsRoundTripAndOverlap(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)

	a := &model.Event{Kind: model.EventOutage, Scope: "ip4", Class: model.ClassISPEdge, StartedAt: 100,
		Details: map[string]any{"down_ticks": 3}}
	if err := s.InsertEvent(ctx, a); err != nil || a.ID == 0 {
		t.Fatalf("insert: id=%d err=%v", a.ID, err)
	}
	a.EndedAt, a.Class = i64(160), model.ClassUpstream
	a.Details = map[string]any{"down_ticks": 60}
	if err := s.UpdateEvent(ctx, *a); err != nil {
		t.Fatal(err)
	}
	b := &model.Event{Kind: model.EventDegraded, Scope: "ip4", StartedAt: 500} // still open
	if err := s.InsertEvent(ctx, b); err != nil {
		t.Fatal(err)
	}

	got, err := s.Events(ctx, 0, 1000, "")
	if err != nil || len(got) != 2 {
		t.Fatalf("all events: %v %v", got, err)
	}
	if got[0].Class != model.ClassUpstream || *got[0].EndedAt != 160 || got[0].Details["down_ticks"] != float64(60) {
		t.Fatalf("updated event = %+v", got[0])
	}
	if got[1].EndedAt != nil || got[1].Details == nil {
		t.Fatalf("open event = %+v", got[1])
	}
	if got, _ := s.Events(ctx, 170, 400, ""); len(got) != 0 {
		t.Fatalf("nothing overlaps 170..400, got %+v", got)
	}
	if got, _ := s.Events(ctx, 900, 950, ""); len(got) != 1 || got[0].ID != b.ID {
		t.Fatalf("open event overlaps later ranges, got %+v", got)
	}
	if got, _ := s.Events(ctx, 0, 1000, model.EventOutage); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("kind filter, got %+v", got)
	}
	if err := s.UpdateEvent(ctx, model.Event{ID: 999}); err == nil {
		t.Fatal("updating a missing event should fail")
	}
}

func TestCloseDanglingEvents(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if err := s.WriteMinutes(ctx, []model.MinuteStat{{TargetID: 1, TS: 600, Sent: 1, Recv: 1}}); err != nil {
		t.Fatal(err)
	}
	ev := &model.Event{Kind: model.EventOutage, Scope: "ip4", StartedAt: 630, Details: map[string]any{"x": 1}}
	late := &model.Event{Kind: model.EventOutage, Scope: "ip4", StartedAt: 900}
	s.InsertEvent(ctx, ev)
	s.InsertEvent(ctx, late)
	n, err := s.CloseDanglingEvents(ctx)
	if err != nil || n != 2 {
		t.Fatalf("closed %d, %v", n, err)
	}
	got, _ := s.Events(ctx, 0, 2000, "")
	if *got[0].EndedAt != 660 || got[0].Details["interrupted"] != true || got[0].Details["x"] != float64(1) {
		t.Fatalf("event = %+v", got[0])
	}
	if *got[1].EndedAt != 900 { // never before its own start
		t.Fatalf("late event ended %d", *got[1].EndedAt)
	}
	if first, _ := s.FirstDataTS(ctx); first != 600 {
		t.Fatalf("first data = %d", first)
	}
}

func TestOutbox(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	id1, _ := s.EnqueueAlert(ctx, "outage_started", []byte(`{"a":1}`), 100)
	id2, _ := s.EnqueueAlert(ctx, "test", []byte(`{}`), 101)

	due, err := s.DueAlerts(ctx, 101, 10)
	if err != nil || len(due) != 2 || due[0].ID != id1 || string(due[0].Payload) != `{"a":1}` {
		t.Fatalf("due = %+v, %v", due, err)
	}
	if err := s.RetryAlert(ctx, id1, 1, 200, "offline"); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueAlerts(ctx, 150, 10); len(due) != 1 || due[0].ID != id2 {
		t.Fatalf("retrying alert should not be due yet: %+v", due)
	}
	s.MarkAlertSent(ctx, id2, 150)
	if ok, _ := s.SupersedeAlert(ctx, id2); ok {
		t.Fatal("a sent alert cannot be superseded")
	}
	if ok, _ := s.SupersedeAlert(ctx, id1); !ok {
		t.Fatal("a pending alert should be superseded")
	}
	if due, _ := s.DueAlerts(ctx, 1000, 10); len(due) != 0 {
		t.Fatalf("nothing should be due: %+v", due)
	}
}

// TestUpgradeFromV1 opens a database created by the M1 schema and checks
// the new tables are added without touching existing data.
func TestUpgradeFromV1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0] + `; PRAGMA user_version = 1;
		INSERT INTO probe_minute (target_id, ts, sent, recv) VALUES (1, 60, 60, 60);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if first, _ := s.FirstDataTS(ctx); first != 60 {
		t.Fatal("existing data lost")
	}
	if err := s.InsertEvent(ctx, &model.Event{Kind: "outage", Scope: "ip4", StartedAt: 1}); err != nil {
		t.Fatalf("events table missing after upgrade: %v", err)
	}
}
