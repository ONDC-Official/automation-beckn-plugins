package fdgate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestReplaySameBodyServesStoredResponse(t *testing.T) {
	tg := newNPGate(t)
	in := ackInner()
	body := confirmMsg("m-replay").bytes(t)

	first := tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	// The participant retries the identical message, freshly signed.
	second := tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{created: testNow.Unix(), expires: testNow.Unix() + 60}))
	if second.Code != first.Code || second.Body.String() != first.Body.String() || second.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("replay differs:\n first %d %s\nsecond %d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if !second.Flushed {
		t.Fatal("replayed response not flushed")
	}
	if in.calls.Load() != 1 {
		t.Fatalf("inner chain ran %d times", in.calls.Load())
	}
	select {
	case n := <-tg.notifier.ch:
		want := ReplayNotice{Domain: domain, Version: version, Action: "application_confirm", MessageID: "m-replay",
			TransactionID: transactionID, SubscriberURL: appURI}
		if n != want {
			t.Fatalf("notice\n got %+v\nwant %+v", n, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("playground not notified")
	}
	// A third arrival is another replay, and another notice.
	tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))
	<-tg.notifier.ch
	if in.calls.Load() != 1 || tg.notifier.count() != 2 {
		t.Fatalf("calls=%d notices=%d", in.calls.Load(), tg.notifier.count())
	}

	// The pending marker is short-lived; the stored response lives for replayTTL.
	key := ReplayKey(domain, appID, "application_confirm", "m-replay")
	tg.store.mu.Lock()
	ttls := append([]time.Duration(nil), tg.store.ttls[key]...)
	tg.store.mu.Unlock()
	if len(ttls) != 2 || ttls[0] != ReplayPendingTTL || ttls[1] != DefaultReplayTTL {
		t.Fatalf("replay key TTLs %v", ttls)
	}
	// The object index uses objectTTL.
	if got := tg.store.ttls[ObjectKey(domain, "applicationId", "APP-1")]; len(got) != 1 || got[0] != DefaultObjectTTL {
		t.Fatalf("index TTLs %v", got)
	}

	// The stored record has the documented shape.
	data, _ := tg.store.get(key)
	var rec map[string]any
	_ = json.Unmarshal(data, &rec)
	if rec["bodySha256"] != bodySHA256(body) || rec["status"] != float64(202) || rec["body"] != first.Body.String() ||
		rec["headers"].(map[string]any)["Content-Type"] != "application/json" || rec["at"] == "" || rec["pending"] != nil {
		t.Fatalf("record %s", data)
	}
}

func TestReplayDifferentBodyIsMessageIDReused(t *testing.T) {
	tg := newNPGate(t)
	in := ackInner()
	m := confirmMsg("m-reuse")
	tg.serve(in, npRequest(t, "POST", confirmTarget, m.bytes(t), signOpts{}))
	m["message"] = map[string]any{"confirm": false}
	b := expectNack(t, tg.serve(in, npRequest(t, "POST", confirmTarget, m.bytes(t), signOpts{})), http.StatusOK, CodeMessageIDReused)
	if b.Error.FieldPath != "/context/message_id" || in.calls.Load() != 1 || tg.notifier.count() != 0 {
		t.Fatalf("error %+v calls %d", b.Error, in.calls.Load())
	}
}

func TestReplayIsScopedBySenderAndAction(t *testing.T) {
	tg := newNPGate(t)
	tg.keys.keys["other.app|k"] = otherKeys.pub
	in := ackInner()
	body := confirmMsg("m-scope").bytes(t)
	tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))

	// Same message_id from another sender: a different key, handled afresh.
	m := confirmMsg("m-scope")
	m.ctx()["bap_id"] = "other.app"
	rec := tg.serve(in, npRequest(t, "POST", confirmTarget, m.bytes(t), signOpts{sub: "other.app", ukid: "k", priv: otherKeys.priv}))
	if rec.Code != http.StatusAccepted || in.calls.Load() != 2 {
		t.Fatalf("%d calls=%d", rec.Code, in.calls.Load())
	}
	// Same sender and message_id, another action: handled afresh.
	cw := newMsg("consent_withdraw", "INVESTOR_APP", "m-scope", nil)
	rec = tg.serve(in, npRequest(t, "POST", prefix+"/consents/C-1/withdraw", cw.bytes(t), signOpts{}))
	if rec.Code != http.StatusAccepted || in.calls.Load() != 3 {
		t.Fatalf("%d calls=%d", rec.Code, in.calls.Load())
	}
}

func TestReplayStoresNacksWithoutNotifying(t *testing.T) {
	tg := newNPGate(t)
	in := &inner{body: becknNACK}
	body := confirmMsg("m-nack-replay").bytes(t)
	first := tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))
	second := tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))
	if first.Body.String() != second.Body.String() || in.calls.Load() != 1 {
		t.Fatalf("NACK not replayed: %s / %s", first.Body.String(), second.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	if tg.notifier.count() != 0 {
		t.Fatal("a refused request has no callback to re-send")
	}
}

func TestReplayOfSyncRequestIsNotNotified(t *testing.T) {
	tg := newNPGate(t)
	in := &inner{body: `{"context":{},"fdEnvelope":{},"message":{"quote":{}}}`}
	body := newMsg("quote_request", "INVESTOR_APP", "m-sync", nil).bytes(t)
	tg.serve(in, npRequest(t, "POST", prefix+"/quotes", body, signOpts{}))
	rec := tg.serve(in, npRequest(t, "POST", prefix+"/quotes", body, signOpts{}))
	if rec.Code != http.StatusOK || in.calls.Load() != 1 {
		t.Fatalf("%d calls=%d", rec.Code, in.calls.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if tg.notifier.count() != 0 {
		t.Fatal("SYNC replay notified")
	}
}

func TestReplayConcurrentDuplicates(t *testing.T) {
	const n = 8
	tg := newNPGate(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	in := ackInner()
	in.before = func() {
		close(entered)
		<-release
	}
	body := confirmMsg("m-burst").bytes(t)
	key := ReplayKey(domain, appID, "application_confirm", "m-burst")

	// Count the duplicates' looks at the pending marker.
	pendingSeen := make(chan struct{}, 1024)
	tg.store.onGet = func(k string, v []byte) {
		var rec replayRecord
		if k == key && json.Unmarshal(v, &rec) == nil && rec.Pending {
			pendingSeen <- struct{}{}
		}
	}

	results := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0] = tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))
	}()
	<-entered
	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))
		}(i)
	}
	// Wait until the duplicates have polled the pending marker for a few rounds (each polls
	// every ReplayPoll), so every one of them is waiting; then let the original finish.
	for seen := 0; seen < 3*(n-1); {
		select {
		case <-pendingSeen:
			seen++
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d duplicates waiting", seen)
		}
	}
	close(release)
	wg.Wait()

	if in.calls.Load() != 1 {
		t.Fatalf("inner chain ran %d times", in.calls.Load())
	}
	for i, r := range results {
		if r.Code != http.StatusAccepted || r.Body.String() != results[0].Body.String() {
			t.Fatalf("response %d differs: %d %s", i, r.Code, r.Body.String())
		}
	}
	time.Sleep(50 * time.Millisecond)
	if tg.notifier.count() != 0 {
		t.Fatalf("duplicates of an in-flight request notified the playground %d times", tg.notifier.count())
	}
}

func TestReplayPendingTimeout(t *testing.T) {
	tg := newNPGate(t)
	tg.g.replayWait = 100 * time.Millisecond
	body := confirmMsg("m-slow").bytes(t)
	// The original is still being handled: its pending marker is in the store.
	pending, _ := json.Marshal(replayRecord{BodySHA256: bodySHA256(body), Pending: true, At: "x"})
	if _, err := tg.store.SetNX(context.Background(), ReplayKey(domain, appID, "application_confirm", "m-slow"), pending, time.Hour); err != nil {
		t.Fatal(err)
	}
	b := expectNack(t, tg.serve(mustNotRun, npRequest(t, "POST", confirmTarget, body, signOpts{})), http.StatusOK, CodeInternal)
	if b.Error.Message == "" {
		t.Fatal("no message")
	}
}

func TestReplayKeyReleasedByOwnerIsReclaimed(t *testing.T) {
	// A duplicate that finds the key gone (the original released it) claims it itself.
	tg := newNPGate(t)
	body := confirmMsg("m-release").bytes(t)
	key := ReplayKey(domain, appID, "application_confirm", "m-release")
	pending, _ := json.Marshal(replayRecord{BodySHA256: bodySHA256(body), Pending: true, At: "x"})
	_, _ = tg.store.SetNX(context.Background(), key, pending, time.Hour)
	once := sync.Once{}
	tg.store.onGet = func(k string, _ []byte) {
		if k == key {
			once.Do(func() { go func() { _ = tg.store.Del(context.Background(), key) }() })
		}
	}
	in := ackInner()
	rec := tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))
	if rec.Code != http.StatusAccepted || in.calls.Load() != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPNotifier(t *testing.T) {
	var gotPath, gotCT string
	var got ReplayNotice
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCT = r.URL.Path, r.Header.Get("Content-Type")
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	n := &HTTPNotifier{MockServiceURL: srv.URL + "/mock/"}
	if u := n.ReplayURL(domain, version); u != srv.URL+"/mock/ONDC:FIS14:FD/0.1.4/fd/replay" {
		t.Fatalf("url %s", u)
	}
	notice := ReplayNotice{Domain: domain, Version: version, Action: "application_confirm", MessageID: "m", TransactionID: "t", SubscriberURL: appURI}
	if err := n.NotifyReplay(context.Background(), notice); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/mock/ONDC:FIS14:FD/0.1.4/fd/replay" || gotCT != "application/json" || got != notice {
		t.Fatalf("path %s ct %s notice %+v", gotPath, gotCT, got)
	}
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer failing.Close()
	if err := (&HTTPNotifier{MockServiceURL: failing.URL}).NotifyReplay(context.Background(), notice); err == nil {
		t.Fatal("want error on HTTP 500")
	}
}

func TestReplayWithoutNotifier(t *testing.T) {
	c := baseConfig(t, ModeNP, prefix)
	store := newMemStore()
	g, err := NewGate(c, Deps{Store: store, Keys: newFakeKeys(), Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	tg := &testGate{g: g, store: store}
	in := ackInner()
	body := confirmMsg("m-nonotify").bytes(t)
	tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{}))
	if rec := tg.serve(in, npRequest(t, "POST", confirmTarget, body, signOpts{})); rec.Code != http.StatusAccepted || in.calls.Load() != 1 {
		t.Fatalf("%d calls=%d", rec.Code, in.calls.Load())
	}
}

// An asynchronous request reaches the playground with its body alone (ONIX forwards no
// headers), so its path parameters are left in the store, under its transaction and message.
func TestNPRecordsPathParamsForThePlayground(t *testing.T) {
	tg := newNPGate(t)
	in := ackInner()
	cw := newMsg("consent_withdraw", "INVESTOR_APP", "m-path", nil)
	rec := tg.serve(in, npRequest(t, "POST", prefix+"/consents/CNS-4/withdraw", cw.bytes(t), signOpts{}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	data, ok := tg.store.get(PathParamsKey(domain, transactionID, "m-path"))
	if !ok || string(data) != `{"consentId":"CNS-4"}` {
		t.Fatalf("path params = %q, %v", data, ok)
	}
	// A request with no path parameters leaves nothing.
	tg.serve(in, npRequest(t, "POST", prefix+"/applications", newMsg("application_create", "INVESTOR_APP", "m-nopath", nil).bytes(t), signOpts{}))
	if _, ok := tg.store.get(PathParamsKey(domain, transactionID, "m-nopath")); ok {
		t.Fatal("recorded empty path parameters")
	}
}
