package speedtest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCloudflare implements __down, __up and /cdn-cgi/trace locally.
func fakeCloudflare(t *testing.T, upBytes *atomic.Int64) *httptest.Server {
	chunk := make([]byte, 32<<10)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cdn-cgi/trace":
			io.WriteString(w, "fl=1\ncolo=DEN\nip=203.0.113.7\n")
		case "/__down":
			n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
			w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
			for n > 0 {
				k := min(n, int64(len(chunk)))
				if _, err := w.Write(chunk[:k]); err != nil {
					return
				}
				n -= k
			}
		case "/__up":
			n, _ := io.Copy(io.Discard, r.Body)
			upBytes.Add(n)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRun(t *testing.T) {
	var upBytes atomic.Int64
	srv := fakeCloudflare(t, &upBytes)
	var pings atomic.Int64
	ping := func(ctx context.Context) (time.Duration, bool) {
		pings.Add(1)
		return 10 * time.Millisecond, true
	}
	cfg := Config{BaseURL: srv.URL, Duration: 500 * time.Millisecond, Streams: 2, ChunkBytes: 4 << 20,
		PingEvery: 50 * time.Millisecond, IdleFor: 200 * time.Millisecond}
	r := New(cfg, ping, srv.Client().Transport.(*http.Transport).TLSClientConfig)

	res, err := r.Run(context.Background(), "manual")
	if err != nil {
		t.Fatalf("run: %v (%+v)", err, res)
	}
	if res.Server != "DEN" || res.Trigger != "manual" {
		t.Fatalf("metadata = %+v", res)
	}
	if res.DownMbps == nil || *res.DownMbps <= 0 || res.UpMbps == nil || *res.UpMbps <= 0 {
		t.Fatalf("throughput = %+v", res)
	}
	if res.BytesDown <= 0 || res.BytesUp <= 0 || upBytes.Load() == 0 {
		t.Fatalf("bytes = %d / %d (server saw %d)", res.BytesDown, res.BytesUp, upBytes.Load())
	}
	if res.IdleMs == nil || *res.IdleMs != 10 || res.LoadedDownMs == nil || res.Grade != "A+" {
		t.Fatalf("latency = idle %v down %v grade %q", res.IdleMs, res.LoadedDownMs, res.Grade)
	}
	if pings.Load() < 10 {
		t.Fatalf("only %d latency samples", pings.Load())
	}
	if p := r.Progress(); p.Running {
		t.Fatal("progress should reset after the run")
	}
}

func TestRunOneAtATime(t *testing.T) {
	var upBytes atomic.Int64
	srv := fakeCloudflare(t, &upBytes)
	cfg := Config{BaseURL: srv.URL, Duration: 300 * time.Millisecond, Streams: 1, ChunkBytes: 1 << 20, PingEvery: 50 * time.Millisecond}
	r := New(cfg, nil, srv.Client().Transport.(*http.Transport).TLSClientConfig)
	done := make(chan struct{})
	go func() { r.Run(context.Background(), "manual"); close(done) }()
	time.Sleep(50 * time.Millisecond)
	if !r.Progress().Running {
		t.Fatal("expected a running test")
	}
	if _, err := r.Run(context.Background(), "manual"); err != ErrBusy {
		t.Fatalf("second run: %v, want ErrBusy", err)
	}
	<-done
}

func TestRunFailsCleanly(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	cfg := Config{BaseURL: srv.URL, Duration: 200 * time.Millisecond, Streams: 2, ChunkBytes: 1 << 20, PingEvery: 50 * time.Millisecond}
	r := New(cfg, nil, srv.Client().Transport.(*http.Transport).TLSClientConfig)
	res, err := r.Run(context.Background(), "scheduled")
	if err == nil || res.DownMbps != nil || res.Error == "" {
		t.Fatalf("want failure, got %+v, %v", res, err)
	}
}

func TestGrade(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		idle, down, up *float64
		want           string
	}{
		{f(10), f(12), f(11), "A+"},
		{f(10), f(35), f(12), "A"},
		{f(10), f(12), f(65), "B"},
		{f(10), f(150), nil, "C"},
		{f(10), f(300), f(20), "D"},
		{f(10), f(900), f(20), "F"},
		{nil, f(20), f(20), ""},
		{f(10), nil, nil, ""},
	}
	for _, tt := range tests {
		if got := Grade(tt.idle, tt.down, tt.up); got != tt.want {
			t.Errorf("Grade(%v,%v,%v) = %q, want %q", tt.idle, tt.down, tt.up, got, tt.want)
		}
	}
}

func TestNextRun(t *testing.T) {
	loc := time.FixedZone("MDT", -6*3600)
	at := func(d, h, m int) time.Time { return time.Date(2026, 9, d, h, m, 0, 0, loc) }
	for _, tt := range []struct {
		now  time.Time
		want time.Time
	}{
		{at(26, 3, 0), at(26, 4, 0)},
		{at(26, 4, 0), at(27, 4, 0)}, // exactly now: tomorrow
		{at(26, 22, 0), at(27, 4, 0)},
		{at(30, 23, 59), time.Date(2026, 10, 1, 4, 0, 0, 0, loc)}, // month rollover
	} {
		got, err := NextRun(tt.now, "04:00")
		if err != nil || !got.Equal(tt.want) {
			t.Errorf("NextRun(%v) = %v, %v; want %v", tt.now, got, err, tt.want)
		}
	}
	for _, bad := range []string{"4", "25:00", "04:60", "noon"} {
		if _, err := NextRun(at(26, 3, 0), bad); err == nil {
			t.Errorf("NextRun(%q) should fail", bad)
		}
	}
}
