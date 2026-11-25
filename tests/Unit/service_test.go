package unit_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyz-is/arandu-whisper"
)

const tokenOne = "0123456789abcdef0123456789abcdef-one"
const tokenTwo = "fedcba9876543210fedcba9876543210-two"

func newHandler(t *testing.T, tr whisper.Transcriber, mod func(*whisper.HandlerConfig)) *whisper.Handler {
	t.Helper()
	cfg := whisper.HandlerConfig{Transcriber: tr, Tokens: map[string]string{"corujao": tokenOne, "other": tokenTwo}}
	if mod != nil {
		mod(&cfg)
	}
	h, err := whisper.NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func post(h http.Handler, token string, body []byte, mime string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/transcribe", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", mime)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerTranscribesWithAValidToken(t *testing.T) {
	fake := whisper.NewFake(whisper.Answer{Text: "olá mundo", Duration: 3 * time.Second})
	h := newHandler(t, fake, nil)
	rec := post(h, tokenTwo, []byte("ogg"), "audio/ogg; codecs=opus")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"text":"olá mundo"`) || !strings.Contains(rec.Body.String(), `"duration_ms":3000`) {
		t.Fatalf("body %s", rec.Body)
	}
	if got := fake.Requests()[0].MIMEType; got != "audio/ogg; codecs=opus" {
		t.Fatalf("the audio's type was lost: %q", got)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("answers must not be cached")
	}
}

func TestHandlerRefusesWithoutAValidToken(t *testing.T) {
	fake := whisper.NewFake(whisper.Answer{Text: "x"})
	h := newHandler(t, fake, nil)
	for _, token := range []string{"", "wrong", tokenOne + "x", tokenOne[:len(tokenOne)-1]} {
		if rec := post(h, token, []byte("a"), "audio/ogg"); rec.Code != 401 {
			t.Fatalf("token %q: status %d", token, rec.Code)
		}
	}
	if len(fake.Requests()) != 0 {
		t.Fatal("an unauthenticated call reached the transcriber")
	}
	req := httptest.NewRequest(http.MethodGet, "/transcribe", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Fatalf("GET: %d", rec.Code)
	}
}

func TestHandlerMapsEveryErrorToAStatusAndNeverEchoesTheCause(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{whisper.ErrEmptyAudio, 400, "empty_audio"},
		{whisper.ErrTooLarge, 413, "too_large"},
		{whisper.ErrTooLong, 422, "too_long"},
		{whisper.ErrUnsupportedAudio, 422, "unsupported_audio"},
		{whisper.ErrTimeout, 504, "timeout"},
		{whisper.ErrBadOutput, 502, "bad_output"},
		{fmt.Errorf("%w: segredo do paciente", whisper.ErrFailed), 502, "failed"},
		{errors.New("segredo do paciente"), 502, "failed"},
	}
	for _, c := range cases {
		h := newHandler(t, whisper.NewFake(whisper.Answer{Err: c.err}), nil)
		rec := post(h, tokenOne, []byte("a"), "audio/ogg")
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), `"error":"`+c.code+`"`) {
			t.Fatalf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "segredo") {
			t.Fatalf("the cause leaked: %s", rec.Body)
		}
	}
}

func TestHandlerLimitsTheBodyBeforeAndWhileReading(t *testing.T) {
	fake := whisper.NewFake(whisper.Answer{Text: "x"}, whisper.Answer{Text: "x"})
	h := newHandler(t, fake, func(c *whisper.HandlerConfig) { c.MaxBytes = 10 })
	if rec := post(h, tokenOne, bytes.Repeat([]byte("a"), 11), "audio/ogg"); rec.Code != 413 {
		t.Fatalf("declared length: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(strings.NewReader(strings.Repeat("a", 50))))
	req.ContentLength = -1 // chunked: the length is not declared
	req.Header.Set("Authorization", "Bearer "+tokenOne)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("undeclared length: %d", rec.Code)
	}
	if len(fake.Requests()) != 0 {
		t.Fatal("an oversize body reached the transcriber")
	}
}

// slow blocks every call until released.
type slow struct {
	release chan struct{}
	started atomic.Int64
}

func (s *slow) Transcribe(ctx context.Context, _ whisper.Audio) (whisper.Transcript, error) {
	s.started.Add(1)
	select {
	case <-s.release:
		return whisper.Transcript{Text: "ok"}, nil
	case <-ctx.Done():
		return whisper.Transcript{}, ctx.Err()
	}
}

func TestHandlerAnswersBusyWhenEverySlotIsTaken(t *testing.T) {
	tr := &slow{release: make(chan struct{})}
	h := newHandler(t, tr, func(c *whisper.HandlerConfig) { c.MaxConcurrent = 1; c.QueueWait = 30 * time.Millisecond })
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); post(h, tokenOne, []byte("a"), "audio/ogg") }()
	for tr.started.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	rec := post(h, tokenOne, []byte("a"), "audio/ogg")
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	close(tr.release)
	wg.Wait()
	if h.InFlight() != 0 {
		t.Fatalf("InFlight %d", h.InFlight())
	}
}

func TestHandlerLogsTheClientAndNeverTheContent(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	h := newHandler(t, whisper.NewFake(whisper.Answer{Text: "dado sigiloso"}), func(c *whisper.HandlerConfig) { c.Logger = log })
	post(h, tokenOne, []byte("audio sigiloso"), "audio/ogg")
	out := logs.String()
	if !strings.Contains(out, "client=corujao") || !strings.Contains(out, "status=200") {
		t.Fatalf("log: %s", out)
	}
	for _, leak := range []string{"sigiloso", tokenOne} {
		if strings.Contains(out, leak) {
			t.Fatalf("the log carries %q: %s", leak, out)
		}
	}
}

func TestNewHandlerValidates(t *testing.T) {
	fake := whisper.NewFake()
	for name, cfg := range map[string]whisper.HandlerConfig{
		"no transcriber": {Tokens: map[string]string{"a": tokenOne}},
		"no tokens":      {Transcriber: fake},
		"short token":    {Transcriber: fake, Tokens: map[string]string{"a": "short"}},
		"no name":        {Transcriber: fake, Tokens: map[string]string{" ": tokenOne}},
		"negative":       {Transcriber: fake, Tokens: map[string]string{"a": tokenOne}, MaxBytes: -1},
	} {
		if _, err := whisper.NewHandler(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// node is a stand-in server with a script of statuses.
type node struct {
	srv    *httptest.Server
	calls  atomic.Int64
	health atomic.Int64
	mu     sync.Mutex
	mode   string // "ok", "fail", "busy", "toolong", "hang"
}

func newNode(t *testing.T, name string) *node {
	t.Helper()
	n := &node{mode: "ok"}
	mux := http.NewServeMux()
	mux.HandleFunc("/transcribe", func(w http.ResponseWriter, r *http.Request) {
		n.calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") != "Bearer "+tokenOne {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		n.mu.Lock()
		mode := n.mode
		n.mu.Unlock()
		switch mode {
		case "fail":
			w.WriteHeader(502)
			fmt.Fprint(w, `{"error":"failed"}`)
		case "busy":
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":"busy"}`)
		case "toolong":
			w.WriteHeader(422)
			fmt.Fprint(w, `{"error":"too_long"}`)
		case "hang":
			<-r.Context().Done()
		default:
			fmt.Fprintf(w, `{"text":"da %s","language":"pt","duration_ms":1500,"segments":[{"start_ms":0,"end_ms":1500,"text":"da %s"}]}`, name, name)
		}
	})
	mux.HandleFunc("/_arandu/health", func(w http.ResponseWriter, _ *http.Request) { n.health.Add(1) })
	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

func (n *node) set(mode string) { n.mu.Lock(); n.mode = mode; n.mu.Unlock() }

func remote(t *testing.T, mod func(*whisper.RemoteConfig), nodes ...*node) *whisper.Remote {
	t.Helper()
	cfg := whisper.RemoteConfig{Token: tokenOne, Cooldown: time.Hour}
	for i, n := range nodes {
		cfg.Nodes = append(cfg.Nodes, whisper.Node{Name: fmt.Sprintf("n%d", i+1), URL: n.srv.URL})
	}
	if mod != nil {
		mod(&cfg)
	}
	r, err := whisper.NewRemote(cfg)
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	return r
}

var sample = whisper.Audio{Data: []byte("ogg"), MIMEType: "audio/ogg"}

func TestRemoteSpreadsCallsOverTheNodes(t *testing.T) {
	a, b, c := newNode(t, "a"), newNode(t, "b"), newNode(t, "c")
	r := remote(t, nil, a, b, c)
	for range 9 {
		if _, err := r.Transcribe(context.Background(), sample); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []*node{a, b, c} {
		if got := n.calls.Load(); got != 3 {
			t.Fatalf("a node got %d of 9 calls", got)
		}
	}
}

func TestRemoteReturnsTheTranscript(t *testing.T) {
	a := newNode(t, "a")
	tr, err := remote(t, nil, a).Transcribe(context.Background(), sample)
	if err != nil || tr.Text != "da a" || tr.Language != "pt" || tr.Duration != 1500*time.Millisecond || len(tr.Segments) != 1 || tr.Segments[0].End != 1500*time.Millisecond {
		t.Fatalf("%+v %v", tr, err)
	}
}

func TestRemoteFailsOverAndTakesAFailingNodeOutOfTheRotation(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.set("fail")
	r := remote(t, func(c *whisper.RemoteConfig) { c.FailureThreshold = 2 }, a, b)
	for range 10 {
		tr, err := r.Transcribe(context.Background(), sample)
		if err != nil || tr.Text != "da b" {
			t.Fatalf("%+v %v", tr, err)
		}
	}
	// a was tried until it failed twice, then left alone.
	if got := a.calls.Load(); got != 2 {
		t.Fatalf("the failing node was called %d times", got)
	}
	var out whisper.NodeHealth
	for _, h := range r.Health(context.Background()) {
		if h.Name == "n1" {
			out = h
		}
	}
	if out.InRotation || out.Failures < 2 {
		t.Fatalf("health: %+v", out)
	}
}

func TestRemoteBringsANodeBackAfterTheCooldown(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.set("fail")
	r := remote(t, func(c *whisper.RemoteConfig) { c.FailureThreshold = 1; c.Cooldown = 40 * time.Millisecond }, a, b)
	for range 4 {
		_, _ = r.Transcribe(context.Background(), sample)
	}
	a.set("ok")
	time.Sleep(60 * time.Millisecond)
	seen := false
	for range 4 {
		if tr, _ := r.Transcribe(context.Background(), sample); tr.Text == "da a" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the node never came back")
	}
}

func TestRemoteTriesTheDownNodesWhenAllAreDown(t *testing.T) {
	a := newNode(t, "a")
	a.set("fail")
	r := remote(t, func(c *whisper.RemoteConfig) { c.FailureThreshold = 1 }, a)
	_, err := r.Transcribe(context.Background(), sample)
	if !errors.Is(err, whisper.ErrNoNodes) || !errors.Is(err, whisper.ErrFailed) {
		t.Fatalf("%v", err)
	}
	a.set("ok")
	if tr, err := r.Transcribe(context.Background(), sample); err != nil || tr.Text != "da a" {
		t.Fatalf("the last resort was not tried: %+v %v", tr, err)
	}
}

func TestRemoteSkipsABusyNodeWithoutTakingItOut(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.set("busy")
	r := remote(t, func(c *whisper.RemoteConfig) { c.FailureThreshold = 1 }, a, b)
	for range 6 {
		if tr, err := r.Transcribe(context.Background(), sample); err != nil || tr.Text != "da b" {
			t.Fatalf("%+v %v", tr, err)
		}
	}
	a.set("ok")
	seen := false
	for range 4 {
		if tr, _ := r.Transcribe(context.Background(), sample); tr.Text == "da a" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("a busy node was taken out of the rotation")
	}
}

func TestRemoteDoesNotRetryARefusalAnotherNodeWouldRepeat(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.set("toolong")
	b.set("toolong")
	r := remote(t, nil, a, b)
	_, err := r.Transcribe(context.Background(), sample)
	if !errors.Is(err, whisper.ErrTooLong) {
		t.Fatalf("%v", err)
	}
	if a.calls.Load()+b.calls.Load() != 1 {
		t.Fatal("a refusal was retried")
	}
}

func TestRemoteChecksTheAudioBeforeTheNetwork(t *testing.T) {
	a := newNode(t, "a")
	r := remote(t, func(c *whisper.RemoteConfig) { c.MaxBytes = 4 }, a)
	if _, err := r.Transcribe(context.Background(), whisper.Audio{}); !errors.Is(err, whisper.ErrEmptyAudio) {
		t.Fatalf("%v", err)
	}
	if _, err := r.Transcribe(context.Background(), whisper.Audio{Data: []byte("12345")}); !errors.Is(err, whisper.ErrTooLarge) {
		t.Fatalf("%v", err)
	}
	if a.calls.Load() != 0 {
		t.Fatal("the network was used")
	}
}

func TestRemoteTimesOutAHangingNodeAndTriesTheNext(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.set("hang")
	r := remote(t, func(c *whisper.RemoteConfig) { c.Timeout = 80 * time.Millisecond }, a, b)
	tr, err := r.Transcribe(context.Background(), sample)
	if err != nil || tr.Text != "da b" {
		t.Fatalf("%+v %v", tr, err)
	}
}

func TestRemoteHonoursACancelledContext(t *testing.T) {
	a := newNode(t, "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := remote(t, nil, a).Transcribe(ctx, sample); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestRemoteAWrongTokenIsNotRetriedAndNeverShown(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	r := remote(t, func(c *whisper.RemoteConfig) { c.Token = tokenTwo }, a, b)
	_, err := r.Transcribe(context.Background(), sample)
	if err == nil || strings.Contains(err.Error(), tokenTwo) || a.calls.Load()+b.calls.Load() != 1 {
		t.Fatalf("%v", err)
	}
}

func TestRemoteHealthReportsEveryNode(t *testing.T) {
	a := newNode(t, "a")
	dead := newNode(t, "b")
	dead.srv.Close()
	r := remote(t, nil, a, dead)
	h := r.Health(context.Background())
	if len(h) != 2 || !h[0].OK || h[1].OK || h[1].Error == "" {
		t.Fatalf("%+v", h)
	}
}

func TestNewRemoteValidates(t *testing.T) {
	good := []whisper.Node{{Name: "a", URL: "https://a.example"}}
	for name, cfg := range map[string]whisper.RemoteConfig{
		"no nodes":    {Token: tokenOne},
		"short token": {Nodes: good, Token: "x"},
		"bad url":     {Nodes: []whisper.Node{{Name: "a", URL: "ftp://a"}}, Token: tokenOne},
		"credentials": {Nodes: []whisper.Node{{Name: "a", URL: "https://u:p@a.example"}}, Token: tokenOne},
		"repeated":    {Nodes: append(good, good...), Token: tokenOne},
		"negative":    {Nodes: good, Token: tokenOne, Cooldown: -1},
	} {
		if _, err := whisper.NewRemote(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseNodes(t *testing.T) {
	got, err := whisper.ParseNodes("one=https://one.example, two=https://two.example:8443/")
	if err != nil || len(got) != 2 || got[0].Name != "one" || got[1].URL != "https://two.example:8443/" {
		t.Fatalf("%+v %v", got, err)
	}
	got, _ = whisper.ParseNodes("https://a.example,https://b.example")
	if got[0].Name != "node1" || got[1].Name != "node2" || got[1].URL != "https://b.example" {
		t.Fatalf("%+v", got)
	}
	got, _ = whisper.ParseNodes("https://only.example")
	if len(got) != 1 || got[0].URL != "https://only.example" {
		t.Fatalf("%+v", got)
	}
	if _, err := whisper.ParseNodes(" , "); err == nil {
		t.Fatal("an empty list was accepted")
	}
}

// The two halves together: Remote against a real Handler.
func TestRemoteAgainstARealHandler(t *testing.T) {
	fake := whisper.NewFake(whisper.Answer{Text: "ponta a ponta", Duration: 2 * time.Second})
	mux := http.NewServeMux()
	mux.Handle("/transcribe", newHandler(t, fake, nil))
	real := httptest.NewServer(mux)
	defer real.Close()
	r, err := whisper.NewRemote(whisper.RemoteConfig{Nodes: []whisper.Node{{Name: "one", URL: real.URL}}, Token: tokenOne})
	if err != nil {
		t.Fatal(err)
	}
	tr, err := r.Transcribe(context.Background(), whisper.Audio{Data: []byte("x"), MIMEType: "audio/ogg"})
	if err != nil || tr.Text != "ponta a ponta" || tr.Duration != 2*time.Second {
		t.Fatalf("%+v %v", tr, err)
	}
	fake.Push(whisper.Answer{Err: whisper.ErrTooLong})
	if _, err := r.Transcribe(context.Background(), whisper.Audio{Data: []byte("x")}); !errors.Is(err, whisper.ErrTooLong) {
		t.Fatalf("%v", err)
	}
}

// The resident whisper-server: the audio is converted here and posted there.
func TestServerURLSendsTheConvertedAudioToTheResidentServer(t *testing.T) {
	var gotForm struct {
		sync.Mutex
		file     int
		language string
		format   string
		prompt   string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inference" {
			w.WriteHeader(404)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(400)
			return
		}
		f, _, _ := r.FormFile("file")
		b, _ := io.ReadAll(f)
		gotForm.Lock()
		gotForm.file = len(b)
		gotForm.language = r.FormValue("language")
		gotForm.format = r.FormValue("response_format")
		gotForm.prompt = r.FormValue("prompt")
		gotForm.Unlock()
		fmt.Fprint(w, `{"language":"portuguese","text":" a ferrugem","segments":[{"start":0,"end":2.5,"text":" A ferrugem"},{"start":2.5,"end":3,"text":" [BLANK_AUDIO]"}]}`)
	}))
	defer srv.Close()

	r := newRig(t, "5.0", "")
	r.cfg.BinaryPath, r.cfg.ModelPath = "", ""
	r.cfg.ServerURL = srv.URL + "/"
	r.cfg.Prompt = "talhão"
	r.cfg.Language = "auto"
	c := r.client(t)
	tr, err := c.Transcribe(context.Background(), whisper.Audio{Data: []byte("opus"), MIMEType: "audio/ogg"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Text != "A ferrugem" || tr.Language != "pt" || tr.Duration != 5*time.Second || len(tr.Segments) != 1 || tr.Segments[0].End != 2500*time.Millisecond {
		t.Fatalf("%+v", tr)
	}
	gotForm.Lock()
	defer gotForm.Unlock()
	if gotForm.file != 5*16000*2+44 || gotForm.language != "auto" || gotForm.format != "verbose_json" || gotForm.prompt != "talhão" {
		t.Fatalf("file=%d language=%q format=%q prompt=%q", gotForm.file, gotForm.language, gotForm.format, gotForm.prompt)
	}
	for _, call := range r.calls(t) {
		if strings.HasPrefix(call, "whisper-cli") {
			t.Fatal("whisper-cli was started although a server is configured")
		}
	}
	r.assertTempDirEmpty(t)
}

func TestServerURLFailureAndBadAnswer(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500); fmt.Fprint(w, "segredo") },
		"junk":   func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "not json") },
	} {
		srv := httptest.NewServer(handler)
		r := newRig(t, "5.0", "")
		r.cfg.ServerURL = srv.URL
		_, err := r.client(t).Transcribe(context.Background(), whisper.Audio{Data: []byte("x")})
		srv.Close()
		if name == "status" && (!errors.Is(err, whisper.ErrFailed) || strings.Contains(err.Error(), "segredo")) {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "junk" && !errors.Is(err, whisper.ErrBadOutput) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A server that is not there.
	r := newRig(t, "5.0", "")
	r.cfg.ServerURL = "http://127.0.0.1:1"
	if _, err := r.client(t).Transcribe(context.Background(), whisper.Audio{Data: []byte("x")}); !errors.Is(err, whisper.ErrFailed) {
		t.Fatalf("%v", err)
	}
}

func TestServerURLIsValidated(t *testing.T) {
	for _, bad := range []string{"ftp://x", "http://", "http://u:p@h", "http://h?x=1", "::"} {
		r := newRig(t, "5.0", "")
		r.cfg.ServerURL = bad
		if _, err := whisper.New(r.cfg); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRemoteWaitsWhenEveryNodeIsBusyAndGoesThroughWhenOneFrees(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.set("busy")
	b.set("busy")
	r := remote(t, func(c *whisper.RemoteConfig) { c.BusyWait = 3 * time.Second }, a, b)
	go func() { time.Sleep(300 * time.Millisecond); b.set("ok") }()
	tr, err := r.Transcribe(context.Background(), sample)
	if err != nil || tr.Text != "da b" {
		t.Fatalf("%+v %v", tr, err)
	}
	if a.calls.Load()+b.calls.Load() < 3 {
		t.Fatal("the round was not repeated")
	}
}

func TestRemoteGivesUpAfterBusyWaitAndCanBeToldNotToWait(t *testing.T) {
	a := newNode(t, "a")
	a.set("busy")
	start := time.Now()
	_, err := remote(t, func(c *whisper.RemoteConfig) { c.BusyWait = 600 * time.Millisecond }, a).Transcribe(context.Background(), sample)
	if !errors.Is(err, whisper.ErrNoNodes) || time.Since(start) < 400*time.Millisecond || time.Since(start) > 3*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
	before := a.calls.Load()
	start = time.Now()
	_, err = remote(t, func(c *whisper.RemoteConfig) { c.BusyWait = -1 }, a).Transcribe(context.Background(), sample)
	if !errors.Is(err, whisper.ErrNoNodes) || a.calls.Load()-before != 1 || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("%v, %d calls, after %s", err, a.calls.Load()-before, time.Since(start))
	}
}

func TestRemoteDoesNotWaitWhenNoNodeIsBusy(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.set("fail")
	b.set("fail")
	start := time.Now()
	_, err := remote(t, func(c *whisper.RemoteConfig) { c.BusyWait = 3 * time.Second }, a, b).Transcribe(context.Background(), sample)
	if !errors.Is(err, whisper.ErrNoNodes) || time.Since(start) > time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
}

func TestRemoteTheWaitStopsWithTheCallersContext(t *testing.T) {
	a := newNode(t, "a")
	a.set("busy")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := remote(t, func(c *whisper.RemoteConfig) { c.BusyWait = time.Minute }, a).Transcribe(ctx, sample)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
}

func TestRemoteStillWaitsForABusyNodeWhenAnotherIsDown(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.set("busy")
	b.set("fail")
	r := remote(t, func(c *whisper.RemoteConfig) { c.BusyWait = 3 * time.Second; c.FailureThreshold = 1 }, a, b)
	go func() { time.Sleep(300 * time.Millisecond); a.set("ok") }()
	start := time.Now()
	if _, err := r.Transcribe(context.Background(), sample); err != nil || time.Since(start) < 200*time.Millisecond {
		t.Fatalf("%v after %s", err, time.Since(start))
	}
}
