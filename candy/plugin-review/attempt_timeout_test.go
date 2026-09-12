package pluginreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAttemptTimeoutKnob: AI_REVIEW_ATTEMPT_TIMEOUT (seconds) overrides the
// default per-attempt HTTP client deadline; invalid values fall back.
func TestAttemptTimeoutKnob(t *testing.T) {
	t.Setenv("AI_REVIEW_ATTEMPT_TIMEOUT", "45")
	cfg, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AttemptTimeout != 45*time.Second {
		t.Errorf("AttemptTimeout: got %v, want 45s", cfg.AttemptTimeout)
	}
	t.Setenv("AI_REVIEW_ATTEMPT_TIMEOUT", "bogus")
	cfg2, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.AttemptTimeout != defaultAttemptTimeout {
		t.Errorf("AttemptTimeout on invalid value: got %v, want default %v", cfg2.AttemptTimeout, defaultAttemptTimeout)
	}
}

// TestTimeoutClassExhaustion: when every attempt dies on the provider-unanswered
// class, the exhausted error is the DISTINCT inconclusive marker — never a
// bare "failed to produce a Verdict line".
func TestTimeoutClassExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		time.Sleep(2 * time.Second) // never answers within the 50ms attempt timeout
	}))
	defer srv.Close()
	cfg := reviewConfig{
		Provider: "test", Model: "m", BaseURL: srv.URL, APIKey: "k",
		MaxTurns: 3, AttemptTimeout: 50 * time.Millisecond,
	}
	_, err := runAgentLoop(context.Background(), cfg, "prompt", toolSet{})
	if err == nil {
		t.Fatal("expected an error from the exhausted loop")
	}
	if !strings.Contains(err.Error(), "inconclusive") {
		t.Errorf("exhaustion error must be the INCONCLUSIVE marker, got: %v", err)
	}
}

// TestTimeoutClassPositive: a non-timeout error is NOT classified as the
// timeout class and does not yield the inconclusive marker.
func TestTimeoutClassPositive(t *testing.T) {
	if !isTimeoutClass(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded must classify as the timeout class")
	}
	if isTimeoutClass(context.Canceled) {
		t.Error("context.Canceled must NOT classify as the timeout class")
	}
}

// TestMaxAttemptsKnob: AI_REVIEW_MAX_ATTEMPTS overrides the default 3-attempt
// loop; invalid values fall back. Zero retries (1) fails hard on the first
// failed attempt instead of cycling.
func TestMaxAttemptsKnob(t *testing.T) {
	cfg, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxAttempts != defaultMaxAttempts {
		t.Errorf("MaxAttempts default: got %d, want %d", cfg.MaxAttempts, defaultMaxAttempts)
	}
	t.Setenv("AI_REVIEW_MAX_ATTEMPTS", "1")
	cfg1, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg1.MaxAttempts != 1 {
		t.Errorf("MaxAttempts on AI_REVIEW_MAX_ATTEMPTS=1: got %d, want 1", cfg1.MaxAttempts)
	}
	t.Setenv("AI_REVIEW_MAX_ATTEMPTS", "bogus")
	cfg2, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.MaxAttempts != defaultMaxAttempts {
		t.Errorf("MaxAttempts on invalid value: got %d, want default %d", cfg2.MaxAttempts, defaultMaxAttempts)
	}
	t.Setenv("AI_REVIEW_MAX_ATTEMPTS", "0")
	cfg3, _, err := parseReviewArgs(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg3.MaxAttempts != defaultMaxAttempts {
		t.Errorf("MaxAttempts on 0: got %d, want default %d", cfg3.MaxAttempts, defaultMaxAttempts)
	}
}
