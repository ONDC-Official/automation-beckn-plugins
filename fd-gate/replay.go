package fdgate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
)

// Replay timing.
const (
	// ReplayWait is how long a duplicate waits for the original's response.
	ReplayWait = 10 * time.Second
	// ReplayPoll is how often a waiting duplicate looks again.
	ReplayPoll = 50 * time.Millisecond
)

// replayRecord is the value under a replay key: a pending marker while the first arrival is
// being handled, then its response.
type replayRecord struct {
	BodySHA256 string            `json:"bodySha256"`
	Pending    bool              `json:"pending,omitempty"`
	Status     int               `json:"status,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	At         string            `json:"at"`
}

// replayOutcome is what the replay check decided for a request.
type replayOutcome int

const (
	replayFirst    replayOutcome = iota // first arrival: handle it, then store the response
	replayStored                        // a stored response was found at once
	replayAwaited                       // the original was in flight; its response arrived
	replayConflict                      // same key, different body
)

// replayClaim is a first arrival's hold on its replay key.
type replayClaim struct {
	g    *Gate
	key  string
	sha  string
	done bool
}

func bodySHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// replayCheck looks a POST up in the replay store. On replayFirst the caller owns claim and
// must finish or release it.
func (g *Gate) replayCheck(ctx context.Context, key string, body []byte) (replayOutcome, *replayRecord, *replayClaim, error) {
	sha := bodySHA256(body)
	pending, err := json.Marshal(replayRecord{BodySHA256: sha, Pending: true, At: g.now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return 0, nil, nil, err
	}
	deadline := time.Now().Add(g.replayWait)
	waited := false
	for {
		ok, err := g.store.SetNX(ctx, key, pending, g.cfg.ReplayTTL)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("replay store: %w", err)
		}
		if ok {
			return replayFirst, nil, &replayClaim{g: g, key: key, sha: sha}, nil
		}
		data, err := g.store.Get(ctx, key)
		if errors.Is(err, ErrNotFound) {
			continue // released (or expired) since SetNX: try to claim it
		}
		if err != nil {
			return 0, nil, nil, fmt.Errorf("replay store: %w", err)
		}
		var rec replayRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return 0, nil, nil, fmt.Errorf("replay store: corrupt record under %s: %w", key, err)
		}
		if rec.BodySHA256 != sha {
			return replayConflict, &rec, nil, nil
		}
		if !rec.Pending {
			if waited {
				return replayAwaited, &rec, nil, nil
			}
			return replayStored, &rec, nil, nil
		}
		if time.Now().After(deadline) {
			return 0, nil, nil, fmt.Errorf("the original request with this message_id is still being handled after %s", g.replayWait)
		}
		waited = true
		select {
		case <-ctx.Done():
			return 0, nil, nil, ctx.Err()
		case <-time.After(ReplayPoll):
		}
	}
}

// finish stores the response for the claim's key, or releases the key when the response
// must not be replayed.
func (c *replayClaim) finish(ctx context.Context, status int, contentType string, body []byte, storable bool) {
	if c == nil || c.done {
		return
	}
	c.done = true
	ctx = context.WithoutCancel(ctx)
	if !storable {
		c.del(ctx)
		return
	}
	rec := replayRecord{
		BodySHA256: c.sha, Status: status, Body: string(body),
		At: c.g.now().UTC().Format(time.RFC3339Nano),
	}
	if contentType != "" {
		rec.Headers = map[string]string{"Content-Type": contentType}
	}
	data, err := json.Marshal(rec)
	if err == nil {
		err = c.g.store.Set(ctx, c.key, data, c.g.cfg.ReplayTTL)
	}
	if err != nil {
		log.Errorf(ctx, err, "fd-gate: cannot store the response under %s; releasing it", c.key)
		c.del(ctx)
	}
}

// release frees the key without storing anything (a panic or an internal failure), so a
// retry is handled afresh.
func (c *replayClaim) release(ctx context.Context) {
	if c == nil || c.done {
		return
	}
	c.done = true
	c.del(context.WithoutCancel(ctx))
}

func (c *replayClaim) del(ctx context.Context) {
	if err := c.g.store.Del(ctx, c.key); err != nil {
		log.Errorf(ctx, err, "fd-gate: cannot release replay key %s", c.key)
	}
}

// ReplayNotice tells the playground that an asynchronous request was replayed, so it re-sends
// the callback it sent for the original.
type ReplayNotice struct {
	Domain        string `json:"domain"`
	Version       string `json:"version"`
	Action        string `json:"action"`
	MessageID     string `json:"message_id"`
	TransactionID string `json:"transaction_id"`
	SubscriberURL string `json:"subscriber_url"`
}

// Notifier delivers replay notices.
type Notifier interface {
	NotifyReplay(ctx context.Context, n ReplayNotice) error
}

// HTTPNotifier posts notices to {MockServiceURL}/{domain}/{version}/fd/replay, the pattern
// workbench-main uses for {mockServiceURL}/{domain}/{version}/manual (domain and version
// unescaped).
type HTTPNotifier struct {
	MockServiceURL string
	Client         *http.Client
}

// ReplayURL is where a notice for domain/version goes.
func (n *HTTPNotifier) ReplayURL(domain, version string) string {
	return strings.TrimRight(n.MockServiceURL, "/") + "/" + domain + "/" + version + "/fd/replay"
}

// NotifyReplay implements Notifier.
func (n *HTTPNotifier) NotifyReplay(ctx context.Context, notice ReplayNotice) error {
	body, err := json.Marshal(notice)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.ReplayURL(notice.Domain, notice.Version), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("replay notice: HTTP %d", resp.StatusCode)
	}
	return nil
}

// notifyReplay sends a notice in the background; the replayed response does not wait for it.
func (g *Gate) notifyReplay(ctx context.Context, n ReplayNotice) {
	if g.notifier == nil {
		log.Warnf(ctx, "fd-gate: replay of %s %s not notified: no mockServiceURL", n.Action, n.MessageID)
		return
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := g.notifier.NotifyReplay(ctx, n); err != nil {
			log.Errorf(ctx, err, "fd-gate: replay notice for %s %s failed", n.Action, n.MessageID)
		}
	}()
}
