package monitor

import (
	"encoding/json"
	"math"
	"strconv"
	"sync"
)

// LiveTick is pushed to /api/stream subscribers after every probe round.
type LiveTick struct {
	TS    int64               `json:"ts"`
	State string              `json:"state"`
	Since int64               `json:"since"`
	RTT   map[string]*float64 `json:"rtt"` // target ID -> ms, null when lost
}

// hub fans live ticks out to subscribers. Slow subscribers miss ticks rather
// than block the probe loop.
type hub struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	closed chan struct{}
}

func newHub() *hub {
	return &hub{subs: make(map[chan []byte]struct{}), closed: make(chan struct{})}
}

func (h *hub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *hub) active() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

func (h *hub) publish(msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (h *hub) close() {
	select {
	case <-h.closed:
	default:
		close(h.closed)
	}
}

// Subscribe returns a channel of JSON-encoded LiveTicks, a channel closed
// at shutdown, and a cancel function.
func (e *Engine) Subscribe() (msgs <-chan []byte, done <-chan struct{}, cancel func()) {
	ch, cancel := e.live.subscribe()
	return ch, e.live.closed, cancel
}

// CloseStreams ends all live streams (so HTTP shutdown doesn't wait on them).
func (e *Engine) CloseStreams() { e.live.close() }

func (e *Engine) publishLive(ts int64, rtts map[int64]*float64) {
	if !e.live.active() {
		return
	}
	e.mu.RLock()
	state, since := e.detState.State, e.detState.Since
	e.mu.RUnlock()
	t := LiveTick{TS: ts, State: state, Since: since, RTT: make(map[string]*float64, len(rtts))}
	for id, v := range rtts {
		if v != nil {
			r := math.Round(*v*1000) / 1000
			v = &r
		}
		t.RTT[strconv.FormatInt(id, 10)] = v
	}
	b, err := json.Marshal(t)
	if err != nil {
		return
	}
	e.live.publish(b)
}
