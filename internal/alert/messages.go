// Package alert turns events into notifications and delivers them through a
// persistent outbox so alerts raised while the internet is down are sent
// once it is back.
package alert

import (
	"fmt"
	"strings"
	"time"

	"github.com/thedatafiend/unraid-internet-monitor/internal/model"
)

// Message is a notifier-independent alert. It is stored as JSON in the outbox.
type Message struct {
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Color       int     `json:"color"`
	Fields      []Field `json:"fields,omitempty"`
	Time        int64   `json:"time"`
}

// Field is a labelled value shown under the description.
type Field struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// Embed colours.
const (
	colorRed   = 0xE5484D
	colorAmber = 0xF5A524
	colorGreen = 0x2EA043
	colorBlue  = 0x3B82F6
)

// Message kinds, stored with each outbox row.
const (
	KindOutageStarted   = "outage_started"
	KindOutageResolved  = "outage_resolved"
	KindDegraded        = "degraded"
	KindDegradedCleared = "degraded_cleared"
	KindDigest          = "digest"
	KindISPHopChange    = "isp_hop_change"
	KindTest            = "test"
)

// ts renders a Discord timestamp, shown in each reader's own time zone.
func ts(unix int64, style string) string { return fmt.Sprintf("<t:%d:%s>", unix, style) }

func fmtDur(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d < time.Minute:
		return d.String()
	case d%time.Minute == 0:
		return strings.TrimSuffix(d.String(), "0s")
	}
	return d.String()
}

func cause(class string) string {
	switch class {
	case model.ClassLocal:
		return "Your router didn't answer either, so the problem is probably on your side: router, modem, cabling or power."
	case model.ClassISPEdge:
		return "Your router answered but your ISP's first router didn't, so the problem is probably your ISP's connection to you."
	case model.ClassUpstream:
		return "Your router and your ISP's first router both answered, so the problem is further inside your ISP's network or beyond."
	}
	return "Unknown."
}

func num(details map[string]any, key string) (float64, bool) {
	switch v := details[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

func outageStarted(ev model.Event, now int64) Message {
	return Message{
		Title:       "🔴 Internet outage",
		Description: fmt.Sprintf("No internet target has answered since %s (%s).", ts(ev.StartedAt, "T"), ts(ev.StartedAt, "R")),
		Color:       colorRed,
		Fields:      []Field{{Name: "Likely cause", Value: cause(ev.Class)}},
		Time:        now,
	}
}

func outageResolved(ev model.Event, now int64) Message {
	end := now
	if ev.EndedAt != nil {
		end = *ev.EndedAt
	}
	m := Message{
		Title: "🟢 Internet restored",
		Description: fmt.Sprintf("Down for **%s**, from %s to %s.",
			fmtDur(end-ev.StartedAt), ts(ev.StartedAt, "f"), ts(end, "T")),
		Color:  colorGreen,
		Fields: []Field{{Name: "Likely cause", Value: cause(ev.Class)}},
		Time:   now,
	}
	if v, ok := num(ev.Details, "gateway_down_pct"); ok {
		m.Fields = append(m.Fields, Field{Name: "Router unreachable", Value: fmt.Sprintf("%.0f%% of the time", v), Inline: true})
	}
	if v, ok := num(ev.Details, "isp_down_pct"); ok {
		m.Fields = append(m.Fields, Field{Name: "ISP edge unreachable", Value: fmt.Sprintf("%.0f%% of the time", v), Inline: true})
	}
	return m
}

func degradedStarted(ev model.Event, now int64) Message {
	loss, _ := num(ev.Details, "loss_pct")
	p95, _ := num(ev.Details, "p95_ms")
	return Message{
		Title:       "🟠 Connection degraded",
		Description: fmt.Sprintf("Sustained packet loss or high latency since %s.", ts(ev.StartedAt, "T")),
		Color:       colorAmber,
		Fields: []Field{
			{Name: "Packet loss (last minute)", Value: fmt.Sprintf("%.1f%%", loss), Inline: true},
			{Name: "p95 latency", Value: fmt.Sprintf("%.0f ms", p95), Inline: true},
		},
		Time: now,
	}
}

func degradedCleared(ev model.Event, now int64) Message {
	end := now
	if ev.EndedAt != nil {
		end = *ev.EndedAt
	}
	loss, _ := num(ev.Details, "peak_loss_pct")
	p95, _ := num(ev.Details, "peak_p95_ms")
	return Message{
		Title:       "🟢 Connection back to normal",
		Description: fmt.Sprintf("Degraded for **%s**, from %s to %s.", fmtDur(end-ev.StartedAt), ts(ev.StartedAt, "f"), ts(end, "T")),
		Color:       colorGreen,
		Fields: []Field{
			{Name: "Worst packet loss", Value: fmt.Sprintf("%.1f%%", loss), Inline: true},
			{Name: "Worst p95 latency", Value: fmt.Sprintf("%.0f ms", p95), Inline: true},
		},
		Time: now,
	}
}

func digestMessage(group string, d *digest, now int64) Message {
	switch group {
	case model.EventOutage:
		noun := "outages"
		if d.count == 1 {
			noun = "outage"
		}
		return Message{
			Title: fmt.Sprintf("🔴 %d more %s", d.count, noun),
			Description: fmt.Sprintf("Between %s and %s: %d %s, **%s** of downtime in total. The connection is up again.",
				ts(d.first, "f"), ts(d.last, "T"), d.count, noun, fmtDur(d.total)),
			Color: colorRed,
			Time:  now,
		}
	default:
		noun := "degraded periods"
		if d.count == 1 {
			noun = "degraded period"
		}
		return Message{
			Title:       fmt.Sprintf("🟠 %d more %s", d.count, noun),
			Description: fmt.Sprintf("Between %s and %s the connection was degraded %d more times.", ts(d.first, "f"), ts(d.last, "T"), d.count),
			Color:       colorAmber,
			Time:        now,
		}
	}
}

func ispHopChanged(oldHop, newHop string, now int64) Message {
	return Message{
		Title:       "🔵 ISP edge router changed",
		Description: fmt.Sprintf("`%s` → `%s`. Your ISP may have rerouted your connection.", oldHop, newHop),
		Color:       colorBlue,
		Time:        now,
	}
}

func testMessage(now int64) Message {
	return Message{
		Title:       "🔵 Test alert",
		Description: "Discord alerts from Internet Monitor are working.",
		Color:       colorBlue,
		Time:        now,
	}
}
