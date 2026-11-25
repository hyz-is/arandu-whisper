package whisper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The values NewRemote applies when RemoteConfig leaves a field at zero.
const (
	// DefaultRemoteTimeout bounds one attempt on one node.
	DefaultRemoteTimeout = 60 * time.Second
	// DefaultFailureThreshold is how many failures in a row take a node out of
	// the rotation.
	DefaultFailureThreshold = 3
	// DefaultCooldown is how long a node stays out of the rotation before it is
	// tried again.
	DefaultCooldown = 30 * time.Second
	// DefaultRemotePath is where Handler is mounted on the node.
	DefaultRemotePath = "/transcribe"
	// DefaultHealthPath is the health endpoint Remote.Health reads.
	DefaultHealthPath = "/_arandu/health"
	// DefaultBusyWait is how long a call keeps trying when every node answered
	// busy.
	DefaultBusyWait = 5 * time.Second
	// busyBackoff is the first pause between two rounds over busy nodes; it
	// doubles up to maxBusyBackoff.
	busyBackoff    = 200 * time.Millisecond
	maxBusyBackoff = time.Second
)

// errNodeBusy marks a node's 429: it is working at its limit, not unwell.
var errNodeBusy = errors.New("busy")

// ErrNoNodes means every node was tried and none answered.
var ErrNoNodes = errors.New("whisper: no node answered")

// Node is one server that runs Handler.
type Node struct {
	// Name identifies the node in health reports and logs. It must not change.
	Name string
	// URL is the node's base address, such as "https://one.api.corujao.ai".
	URL string
}

// ParseNodes reads the nodes from configuration: name=url pairs separated by
// commas ("one=https://one.example,two=https://two.example"), or bare URLs,
// which are named node1, node2 and so on, or a single URL.
func ParseNodes(s string) ([]Node, error) {
	var nodes []Node
	for i, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, raw := fmt.Sprintf("node%d", i+1), item
		if before, after, ok := strings.Cut(item, "="); ok && !strings.Contains(before, "/") && !strings.Contains(before, ":") {
			name, raw = strings.TrimSpace(before), strings.TrimSpace(after)
		}
		nodes = append(nodes, Node{Name: name, URL: raw})
	}
	if len(nodes) == 0 {
		return nil, errors.New("whisper: no node in the list")
	}
	return nodes, nil
}

// RemoteConfig is what NewRemote needs.
type RemoteConfig struct {
	// Nodes are the servers, each running Handler. At least one; names are
	// unique and URLs are http or https. Calls are spread over them in turn,
	// and a node that fails is taken out of the rotation for a while.
	Nodes []Node
	// Token is the bearer token this client was given. Required. It is sent
	// only to the nodes, and never logged or put in an error.
	Token string
	// Path is where Handler is mounted. Empty means DefaultRemotePath.
	Path string
	// HealthPath is the node's health endpoint. Empty means DefaultHealthPath.
	HealthPath string
	// Timeout bounds one attempt on one node. Zero means DefaultRemoteTimeout.
	Timeout time.Duration
	// FailureThreshold is how many failures in a row take a node out of the
	// rotation. Zero means DefaultFailureThreshold.
	FailureThreshold int
	// Cooldown is how long a node stays out. Zero means DefaultCooldown.
	Cooldown time.Duration
	// MaxBytes is the largest audio sent, refused here before any network.
	// Zero means DefaultMaxBytes.
	MaxBytes int64
	// BusyWait is how long a call keeps going round the nodes when every one of
	// them answered 429 (busy, not failing): each round waits a little longer,
	// up to BusyWait in all, and the call fails with ErrNoNodes only after it. A
	// burst of calls than the nodes can take at once then waits its turn instead
	// of failing. Zero means DefaultBusyWait; a negative value turns the wait off
	// and a call fails as soon as one round found every node busy.
	BusyWait time.Duration
	// HTTPClient carries the calls. Nil means a client with a pooled transport
	// and no timeout of its own (Timeout ends each attempt).
	HTTPClient *http.Client
}

// Remote is a Transcriber that calls nodes running Handler. It is what an
// application passes where it passed a *Client: the same interface, so which
// one runs is a matter of configuration. It is safe for concurrent use.
type Remote struct {
	nodes      []*remoteNode
	token      string
	path       string
	healthPath string
	timeout    time.Duration
	threshold  int
	cooldown   time.Duration
	maxBytes   int64
	busyWait   time.Duration
	http       *http.Client
	next       atomic.Uint64
	now        func() time.Time
}

type remoteNode struct {
	Node
	base string

	mu        sync.Mutex
	failures  int
	downUntil time.Time
	lastErr   string
}

var _ Transcriber = (*Remote)(nil)

// NewRemote validates cfg and returns the client. Nothing is called here.
func NewRemote(cfg RemoteConfig) (*Remote, error) {
	if len(cfg.Nodes) == 0 {
		return nil, errors.New("whisper: RemoteConfig.Nodes needs at least one node")
	}
	if len(cfg.Token) < MinTokenLength {
		return nil, fmt.Errorf("whisper: RemoteConfig.Token is shorter than %d characters", MinTokenLength)
	}
	if cfg.Timeout < 0 || cfg.FailureThreshold < 0 || cfg.Cooldown < 0 || cfg.MaxBytes < 0 {
		return nil, errors.New("whisper: RemoteConfig limits must not be negative")
	}
	r := &Remote{
		token:      cfg.Token,
		path:       cfg.Path,
		healthPath: cfg.HealthPath,
		timeout:    cfg.Timeout,
		threshold:  cfg.FailureThreshold,
		cooldown:   cfg.Cooldown,
		maxBytes:   cfg.MaxBytes,
		busyWait:   cfg.BusyWait,
		http:       cfg.HTTPClient,
		now:        time.Now,
	}
	seen := map[string]bool{}
	for _, n := range cfg.Nodes {
		name := strings.TrimSpace(n.Name)
		if name == "" || seen[name] {
			return nil, fmt.Errorf("whisper: node name %q is empty or repeated", n.Name)
		}
		seen[name] = true
		base, err := resolveServerURL(n.URL)
		if err != nil {
			return nil, fmt.Errorf("whisper: node %q URL: %w", name, err)
		}
		r.nodes = append(r.nodes, &remoteNode{Node: Node{Name: name, URL: base}, base: base})
	}
	if r.path == "" {
		r.path = DefaultRemotePath
	}
	if r.healthPath == "" {
		r.healthPath = DefaultHealthPath
	}
	if r.timeout == 0 {
		r.timeout = DefaultRemoteTimeout
	}
	if r.threshold == 0 {
		r.threshold = DefaultFailureThreshold
	}
	if r.cooldown == 0 {
		r.cooldown = DefaultCooldown
	}
	if r.maxBytes == 0 {
		r.maxBytes = DefaultMaxBytes
	}
	if r.busyWait == 0 {
		r.busyWait = DefaultBusyWait
	}
	if r.http == nil {
		r.http = &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		}}
	}
	return r, nil
}

// Transcribe sends the audio to the next healthy node. A node that is busy,
// unreachable, timed out or failing is skipped for this call and, after
// FailureThreshold failures in a row, for Cooldown; the next node is tried at
// once. A refusal that another node would repeat (empty, too large, too long
// or unreadable audio) is returned as it is. When every node failed the error
// wraps ErrNoNodes and the last node's error.
func (r *Remote) Transcribe(ctx context.Context, audio Audio) (Transcript, error) {
	if len(audio.Data) == 0 {
		return Transcript{}, ErrEmptyAudio
	}
	if int64(len(audio.Data)) > r.maxBytes {
		return Transcript{}, fmt.Errorf("%w: %d bytes, the limit is %d", ErrTooLarge, len(audio.Data), r.maxBytes)
	}
	deadline := time.Now().Add(r.busyWait)
	pause := busyBackoff
	for {
		var last error
		allBusy := true
		for _, n := range r.order() {
			if err := ctx.Err(); err != nil {
				return Transcript{}, err
			}
			tr, retry, err := r.call(ctx, n, audio)
			if err == nil {
				n.succeed()
				return tr, nil
			}
			if !retry {
				return Transcript{}, err
			}
			if !errors.Is(err, errNodeBusy) {
				allBusy = false
			}
			last = err
		}
		// Only a round in which every node was busy is worth another: a node
		// that failed was already skipped for the next call, and waiting does
		// not mend it.
		remaining := time.Until(deadline)
		if !allBusy || r.busyWait < 0 || remaining <= 0 {
			return Transcript{}, fmt.Errorf("%w: %w", ErrNoNodes, last)
		}
		select {
		case <-time.After(min(pause, remaining)):
		case <-ctx.Done():
			return Transcript{}, ctx.Err()
		}
		pause = min(pause*2, maxBusyBackoff)
	}
}

// order lists the nodes to try: the ones in the rotation starting at the next
// turn, then the ones taken out, as a last resort, soonest back first.
func (r *Remote) order() []*remoteNode {
	start := int(r.next.Add(1)-1) % len(r.nodes)
	var up, down []*remoteNode
	now := r.now()
	for i := range r.nodes {
		n := r.nodes[(start+i)%len(r.nodes)]
		if n.isDown(now) {
			down = append(down, n)
		} else {
			up = append(up, n)
		}
	}
	// A node's cooldown is short, so the order among the downed ones is the
	// order of the rotation.
	return append(up, down...)
}

func (n *remoteNode) isDown(now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return now.Before(n.downUntil)
}

func (n *remoteNode) succeed() {
	n.mu.Lock()
	n.failures, n.downUntil, n.lastErr = 0, time.Time{}, ""
	n.mu.Unlock()
}

func (r *Remote) fail(n *remoteNode, why string) {
	n.mu.Lock()
	n.failures++
	n.lastErr = why
	if n.failures >= r.threshold {
		n.downUntil = r.now().Add(r.cooldown)
	}
	n.mu.Unlock()
}

// call makes one attempt on one node. retry says whether another node could do
// better.
func (r *Remote) call(ctx context.Context, n *remoteNode, audio Audio) (Transcript, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base+r.path, bytes.NewReader(audio.Data))
	if err != nil {
		return Transcript{}, false, fmt.Errorf("whisper: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	mime := audio.MIMEType
	if mime == "" {
		mime = "application/octet-stream"
	}
	req.Header.Set("Content-Type", mime)
	req.Header.Set("Accept", "application/json")

	resp, err := r.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(ctxErr, context.Canceled) {
			return Transcript{}, false, ctxErr
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			r.fail(n, "timeout")
			return Transcript{}, true, fmt.Errorf("whisper: node %s: %w", n.Name, ErrTimeout)
		}
		r.fail(n, "unreachable")
		return Transcript{}, true, fmt.Errorf("whisper: node %s is unreachable", n.Name)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxServerReply))

	if resp.StatusCode == http.StatusOK {
		var w wireTranscript
		if json.Unmarshal(raw, &w) != nil {
			r.fail(n, "unreadable answer")
			return Transcript{}, true, fmt.Errorf("whisper: node %s: %w", n.Name, ErrBadOutput)
		}
		t := Transcript{Text: w.Text, Language: w.Language, Duration: time.Duration(w.DurationMS) * time.Millisecond}
		for _, s := range w.Segments {
			t.Segments = append(t.Segments, Segment{
				Start: time.Duration(s.StartMS) * time.Millisecond,
				End:   time.Duration(s.EndMS) * time.Millisecond,
				Text:  s.Text,
			})
		}
		return t, false, nil
	}

	var we wireError
	_ = json.Unmarshal(raw, &we)
	switch we.Code {
	case codeEmptyAudio:
		return Transcript{}, false, ErrEmptyAudio
	case codeTooLarge:
		return Transcript{}, false, fmt.Errorf("%w: refused by node %s", ErrTooLarge, n.Name)
	case codeTooLong:
		return Transcript{}, false, fmt.Errorf("%w: refused by node %s", ErrTooLong, n.Name)
	case codeUnsupported:
		return Transcript{}, false, fmt.Errorf("%w: refused by node %s", ErrUnsupportedAudio, n.Name)
	case codeUnauth:
		// A wrong token is wrong on every node, and retrying cannot fix it.
		return Transcript{}, false, fmt.Errorf("whisper: node %s refused the token", n.Name)
	case codeBusy:
		// Busy is not sick: skip the node for this call and leave it in the
		// rotation.
		return Transcript{}, true, fmt.Errorf("whisper: node %s is %w", n.Name, errNodeBusy)
	case codeCanceled:
		return Transcript{}, false, context.Canceled
	case codeTimeout:
		r.fail(n, "timeout")
		return Transcript{}, true, fmt.Errorf("whisper: node %s: %w", n.Name, ErrTimeout)
	}
	r.fail(n, fmt.Sprintf("status %d", resp.StatusCode))
	return Transcript{}, true, fmt.Errorf("whisper: node %s: %w: status %d", n.Name, ErrFailed, resp.StatusCode)
}

// NodeHealth is what Remote.Health reports for one node.
type NodeHealth struct {
	// Name and URL are the node's, as configured.
	Name, URL string
	// OK is true when the node's health endpoint answered 200 just now.
	OK bool
	// InRotation is false while the node is out for failing calls, whatever
	// the health endpoint says.
	InRotation bool
	// Latency is how long the health call took.
	Latency time.Duration
	// Failures are the failures in a row of real calls.
	Failures int
	// Error is a short reason when OK is false, or the last call's failure.
	Error string
}

// Health asks every node's health endpoint, in parallel, and reports each one
// with its place in the rotation. A node that answers 200 here and has been
// failing real calls stays out until its cooldown ends: the endpoint says the
// process is up, not that it transcribes.
func (r *Remote) Health(ctx context.Context) []NodeHealth {
	out := make([]NodeHealth, len(r.nodes))
	var wg sync.WaitGroup
	for i, n := range r.nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := NodeHealth{Name: n.Name, URL: n.URL}
			n.mu.Lock()
			h.Failures, h.Error = n.failures, n.lastErr
			n.mu.Unlock()
			h.InRotation = !n.isDown(r.now())

			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			started := time.Now()
			req, err := http.NewRequestWithContext(cctx, http.MethodGet, n.base+r.healthPath, nil)
			if err == nil {
				var resp *http.Response
				if resp, err = r.http.Do(req); err == nil {
					_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
					resp.Body.Close()
					h.OK = resp.StatusCode == http.StatusOK
					if !h.OK {
						h.Error = fmt.Sprintf("health status %d", resp.StatusCode)
					}
				} else {
					h.Error = "health unreachable"
				}
			}
			h.Latency = time.Since(started)
			out[i] = h
		}()
	}
	wg.Wait()
	return out
}
