package pluginreview

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ---- chat-completions tool-loop client (the 1:1 port of pi-review-action) ----

type toolSchema struct {
	Type     string         `json:"type"`
	Function functionSchema `json:"function"`
}
type functionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

var emptyParams = json.RawMessage(`{"type":"object","properties":{}}`)

// the SAME four read-only tools the action exported — identical names/descriptions.
var reviewTools = []toolSchema{
	{Type: "function", Function: functionSchema{Name: "get_pr_diff", Description: "CURRENT unified diff (head vs base).", Parameters: emptyParams}},
	{Type: "function", Function: functionSchema{Name: "get_pr_commits", Description: "Commit history of this PR (sha, message, author) — read commit messages since the last review here.", Parameters: emptyParams}},
	{Type: "function", Function: functionSchema{Name: "get_pr_thread", Description: "CURRENT live issue body plus all prior comments (older comments are stale until re-verified).", Parameters: emptyParams}},
	{Type: "function", Function: functionSchema{Name: "get_pr_meta", Description: "PR metadata: title, state, mergeable, head/base sha, file counts.", Parameters: emptyParams}},
}

type chatMsg struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type chatRequest struct {
	Model       string       `json:"model"`
	Messages    []chatMsg    `json:"messages"`
	Temperature float64      `json:"temperature"`
	Tools       []toolSchema `json:"tools"`
	ToolChoice  string       `json:"tool_choice"`
	// Stream is always true: the completion is consumed as SSE so the client
	// bounds SILENCE (a chunk that never arrives) instead of total generation
	// time. A whole-generation deadline is what made a large tool-result turn
	// fail with "awaiting headers" (RCA: review timeouts).
	Stream bool `json:"stream"`
}
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   *string    `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

// ---- streaming (SSE) shapes ----

// streamChunk is one OpenAI-compatible SSE chunk: content arrives token by token
// and tool calls arrive as fragments keyed by their delta index.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   *string         `json:"content"`
			ToolCalls []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// streamAccumulator reassembles the assistant message from SSE deltas. Tool-call
// fragments are merged by their delta index (the provider sends the id/name once
// and then streams the JSON arguments in pieces).
type streamAccumulator struct {
	content  strings.Builder
	calls    []toolCall
	byIndex  map[int]int
	finish   string
	sawDelta bool
}

func (a *streamAccumulator) add(payload string) error {
	var ch streamChunk
	if err := json.Unmarshal([]byte(payload), &ch); err != nil {
		return fmt.Errorf("LLM stream chunk decode: %w", err)
	}
	if ch.Error != nil {
		return fmt.Errorf("LLM stream error: %s", ch.Error.Message)
	}
	if len(ch.Choices) > 0 {
		a.sawDelta = true
	}
	for _, c := range ch.Choices {
		if c.Delta.Content != nil {
			a.content.WriteString(*c.Delta.Content)
		}
		for _, tc := range c.Delta.ToolCalls {
			a.addToolCall(tc)
		}
		if c.FinishReason != nil {
			a.finish = *c.FinishReason
		}
	}
	return nil
}

func (a *streamAccumulator) addToolCall(d toolCallDelta) {
	if a.byIndex == nil {
		a.byIndex = map[int]int{}
	}
	pos, ok := a.byIndex[d.Index]
	if !ok {
		a.calls = append(a.calls, toolCall{Type: "function"})
		pos = len(a.calls) - 1
		a.byIndex[d.Index] = pos
	}
	c := &a.calls[pos]
	if d.ID != "" {
		c.ID = d.ID
	}
	if d.Type != "" {
		c.Type = d.Type
	}
	c.Function.Name += d.Function.Name
	c.Function.Arguments += d.Function.Arguments
	if c.Type == "" {
		c.Type = "function"
	}
}

func (a *streamAccumulator) message() (chatMsg, error) {
	if !a.sawDelta {
		return chatMsg{}, fmt.Errorf("LLM response had no choices")
	}
	m := chatMsg{Role: "assistant", ToolCalls: a.calls}
	if a.content.Len() > 0 {
		s := a.content.String()
		m.Content = &s
	}
	return m, nil
}

// ---- client ----

const (
	// llmDialTimeout / llmTLSHandshake bound connection establishment, so a
	// blackholed endpoint fails as a connect error instead of eating the whole
	// generation budget.
	llmDialTimeout  = 30 * time.Second
	llmTLSHandshake = 15 * time.Second
	// llmIdleConnTimeout is a SECOND line of defence for the stale-connection
	// class (DisableKeepAlives is the first): nothing may sit in the pool long
	// enough for the peer to drop it silently.
	llmIdleConnTimeout = 30 * time.Second
	// maxLLMResponseBytes bounds a NON-streaming (JSON fallback) body.
	maxLLMResponseBytes = 8 << 20
	// maxSSELineBytes bounds one SSE line so a malformed stream cannot grow
	// unboundedly.
	maxSSELineBytes = 1 << 20
)

type llmClient struct {
	baseURL      string
	apiKey       string
	model        string
	http         *http.Client
	totalTimeout time.Duration // whole turn request: headers through last streamed chunk
	idleTimeout  time.Duration // maximum silence between streamed chunks

	// sessionID is the per-run session-affinity token. opencode's Go gateway REJECTS a
	// request without it — HTTP 400 MissingSessionID ("cannot be routed efficiently") —
	// and the SAME request returns 200 once `x-opencode-session` is present (verified
	// against the live gateway). One id is minted per review run so every request of
	// that run shares a session; other providers ignore the header.
	sessionID string
}

// newHTTPClient builds the transport EXPLICITLY — the review loop issues a
// handful of POSTs per turn separated by minutes of local tool work (each tool
// call is a separate `gh api` subprocess), so connection REUSE buys nothing and
// is a hang source: a keep-alive connection the peer already dropped silently
// leaves a POST waiting instead of dialling (Go retries only idempotent
// requests, and a POST is not one). DisableKeepAlives therefore dials fresh per
// request; every remaining phase is explicitly bounded.
func newHTTPClient(totalTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: llmDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     true,
		MaxIdleConns:          2,
		IdleConnTimeout:       llmIdleConnTimeout,
		TLSHandshakeTimeout:   llmTLSHandshake,
		ExpectContinueTimeout: 1 * time.Second,
		// time-to-first-byte. A STREAMING request gets its headers as soon as the
		// provider accepts it, so this bounds a dead/blackholed peer without
		// capping the generation itself.
		ResponseHeaderTimeout: totalTimeout,
	}
	// Timeout stays 0: the bound is per-request (a streamed generation is allowed
	// to outlive a client-wide wall clock), applied by chat() as a context.
	return &http.Client{Transport: tr, Timeout: 0}
}

func newLLMClient(cfg reviewConfig) *llmClient {
	total := cfg.AttemptTimeout
	if total <= 0 {
		total = defaultAttemptTimeout
	}
	idle := cfg.StreamIdleTimeout
	if idle <= 0 {
		idle = defaultStreamIdleTimeout
	}
	return &llmClient{
		baseURL:      trimTrailingSlash(cfg.BaseURL),
		apiKey:       cfg.APIKey,
		model:        cfg.Model,
		http:         newHTTPClient(total),
		totalTimeout: total,
		idleTimeout:  idle,
		sessionID:    newSessionID(),
	}
}

// newSessionID mints a per-run session-affinity id (32 lowercase hex chars) with no new
// dependency — crypto/rand + hex is the whole implementation. A rand failure must not fall
// back to a CONSTANT id (every concurrent run would share one session, and an empty value
// would reproduce the very 400 this exists to prevent), so it falls back to a value that is
// still unique per process and call.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("review-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// chat posts one completion step and returns the assistant message.
//
// The request is STREAMED: the response headers arrive immediately and the body
// is consumed chunk by chunk, so the bounds are (1) time to first byte and (2)
// silence between chunks, with the whole turn request capped by totalTimeout.
// A large tool-result context (turn 2) therefore no longer has to finish inside
// one wall-clock deadline — it only has to keep producing output.
//
// chat() does NOT retry on its own: the re-issue policy lives in exactly one
// place (chatTurn in review.go), which retries the FAILED TURN with the same
// conversation state.
func (c *llmClient) chat(ctx context.Context, messages []chatMsg) (chatMsg, error) {
	body, err := json.Marshal(chatRequest{
		Model: c.model, Messages: messages, Temperature: 0.2,
		Tools: reviewTools, ToolChoice: "auto", Stream: true,
	})
	if err != nil {
		return chatMsg{}, err
	}
	if c.totalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.totalTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatMsg{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("HTTP-Referer", "https://github.com/opencharly/action-review")
	req.Header.Set("X-Title", "action-review")
	// Session affinity: gateways that demand it (opencode's Go gateway returns HTTP 400
	// MissingSessionID without it) route on this header; gateways that do not, ignore an
	// unknown header. Sent unconditionally because the header IS the session identity —
	// conditioning it on a provider guess would silently reintroduce the 400 on a base
	// URL the guess misses.
	req.Header.Set("x-opencode-session", c.sessionID)

	resp, err := c.http.Do(req)
	if err != nil {
		return chatMsg{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return chatMsg{}, fmt.Errorf("LLM %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	// A provider/gateway that ignores stream:true answers with a plain JSON body;
	// accept it rather than failing the review.
	if !isEventStream(resp.Header.Get("Content-Type")) {
		return decodeChatResponse(resp.Body)
	}
	return c.consumeStream(ctx, resp.Body)
}

func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// decodeChatResponse parses the non-streaming fallback body.
func decodeChatResponse(body io.Reader) (chatMsg, error) {
	raw, _ := io.ReadAll(io.LimitReader(body, maxLLMResponseBytes))
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return chatMsg{}, fmt.Errorf("LLM response decode: %w", err)
	}
	if len(cr.Choices) == 0 {
		return chatMsg{}, fmt.Errorf("LLM response had no choices")
	}
	m := cr.Choices[0].Message
	return chatMsg{Role: "assistant", Content: m.Content, ToolCalls: m.ToolCalls}, nil
}

// consumeStream reads the SSE body with a bound on SILENCE, not on total
// generation: every chunk resets the idle timer, so a long but LIVE generation
// is allowed to finish while a dead stream is cut at idleTimeout (the outer ctx
// still caps the whole turn request at totalTimeout).
func (c *llmClient) consumeStream(ctx context.Context, body io.Reader) (chatMsg, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idleTimeout := c.idleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamIdleTimeout
	}
	// Unbuffered: scanSSE hands every payload over before it returns, so the
	// scanErr case can never be observed with chunks still in flight.
	payloads := make(chan string)
	scanErr := make(chan error, 1)
	go func() {
		scanErr <- scanSSE(body, func(payload string) error {
			select {
			case payloads <- payload:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	var acc streamAccumulator
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return chatMsg{}, fmt.Errorf("LLM request exceeded the %v whole-request deadline: %w", c.totalTimeout, context.DeadlineExceeded)
			}
			return chatMsg{}, ctx.Err()
		case payload := <-payloads:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleTimeout)
			if err := acc.add(payload); err != nil {
				return chatMsg{}, err
			}
		case err := <-scanErr:
			if err != nil {
				return chatMsg{}, err
			}
			return acc.message()
		case <-idle.C:
			// cancel() (deferred) unblocks scanSSE and releases the connection.
			return chatMsg{}, fmt.Errorf("LLM stream stalled: no chunk for %v (provider stopped streaming; turn context may be large): %w", idleTimeout, context.DeadlineExceeded)
		}
	}
}

// scanSSE is the pure SSE reader: the data lines of one event are joined with
// "\n" and handed to onPayload; a "[DONE]" sentinel ends the stream; comment /
// keep-alive / event-name lines are ignored. onPayload may return an error to
// stop (used to abort on a malformed chunk and to respect cancellation).
func scanSSE(r io.Reader, onPayload func(string) error) error {
	br := bufio.NewReaderSize(r, maxSSELineBytes)
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		joined := strings.Join(data, "\n")
		data = data[:0]
		return onPayload(joined)
	}
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return fmt.Errorf("LLM stream: event line exceeds %d bytes", maxSSELineBytes)
		}
		if len(line) > 0 {
			s := strings.TrimRight(string(line), "\r\n")
			switch {
			case s == "":
				if e := flush(); e != nil {
					return e
				}
			case strings.HasPrefix(s, ":"): // SSE comment / keep-alive
			case strings.HasPrefix(s, "data:"):
				v := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
				if v == "[DONE]" {
					return nil
				}
				data = append(data, v)
			default: // event:/id:/retry: — the payload still arrives via data:
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return flush()
			}
			return err
		}
	}
}
