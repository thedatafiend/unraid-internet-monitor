package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PermanentError means retrying will not help (e.g. the webhook was deleted).
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// RetryAfterError asks the caller to wait before the next attempt.
type RetryAfterError struct {
	After time.Duration
	Err   error
}

func (e *RetryAfterError) Error() string { return e.Err.Error() }
func (e *RetryAfterError) Unwrap() error { return e.Err }

// Discord posts messages to a Discord webhook.
type Discord struct {
	URL    string
	Client *http.Client
}

// NewDiscord returns a sender for webhookURL.
func NewDiscord(webhookURL string) *Discord {
	return &Discord{URL: webhookURL, Client: &http.Client{Timeout: 10 * time.Second}}
}

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
	Footer      map[string]any `json:"footer,omitempty"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// Send delivers m. Errors never include the webhook URL, which is a secret.
func (d *Discord) Send(ctx context.Context, m Message) error {
	embed := discordEmbed{Title: m.Title, Description: m.Description, Color: m.Color,
		Footer: map[string]any{"text": "Internet Monitor"}}
	if m.Time > 0 {
		embed.Timestamp = time.Unix(m.Time, 0).UTC().Format(time.RFC3339)
	}
	for _, f := range m.Fields {
		embed.Fields = append(embed.Fields, discordField(f))
	}
	body, err := json.Marshal(map[string]any{
		"username":         "Internet Monitor",
		"embeds":           []discordEmbed{embed},
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
	if err != nil {
		return &PermanentError{err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return &PermanentError{errors.New("invalid Discord webhook URL")}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL (and its token) from the message
		}
		return fmt.Errorf("discord: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return &RetryAfterError{After: retryAfter(resp.Header, respBody), Err: errors.New("discord: rate limited")}
	case resp.StatusCode >= 500:
		return fmt.Errorf("discord: %s", resp.Status)
	default:
		return &PermanentError{fmt.Errorf("discord: %s: %s", resp.Status, bytes.TrimSpace(respBody))}
	}
}

// retryAfter reads Discord's rate-limit hint from the JSON body or header.
func retryAfter(h http.Header, body []byte) time.Duration {
	var rl struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &rl) == nil && rl.RetryAfter > 0 {
		return time.Duration(rl.RetryAfter * float64(time.Second))
	}
	if s, err := strconv.ParseFloat(h.Get("Retry-After"), 64); err == nil && s > 0 {
		return time.Duration(s * float64(time.Second))
	}
	return 5 * time.Second
}
