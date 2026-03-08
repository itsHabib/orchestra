package activity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// RegisterBuiltins registers all built-in activity handlers on the given registry.
func RegisterBuiltins(r *Registry) {
	r.Register("noop", noopHandler)
	r.Register("sleep", sleepHandler)
	r.Register("http-call", httpCallHandler)
	r.Register("log", logHandler)
	r.Register("fail", failHandler)
}

// noopHandler returns the input as-is.
func noopHandler(_ context.Context, input string) (string, error) {
	return input, nil
}

// sleepHandler parses {"duration_ms": N}, sleeps for that duration, and returns {"slept": N}.
func sleepHandler(ctx context.Context, input string) (string, error) {
	var params struct {
		DurationMS int `json:"duration_ms"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return "", fmt.Errorf("sleep: invalid input: %w", err)
	}

	timer := time.NewTimer(time.Duration(params.DurationMS) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return fmt.Sprintf(`{"slept": %d}`, params.DurationMS), nil
	}
}

// httpCallHandler parses {"url": "...", "method": "GET|POST", "body": "..."}, makes the request,
// and returns {"status": N, "body": "..."}.
func httpCallHandler(ctx context.Context, input string) (string, error) {
	var params struct {
		URL    string `json:"url"`
		Method string `json:"method"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return "", fmt.Errorf("http-call: invalid input: %w", err)
	}
	if params.Method == "" {
		params.Method = "GET"
	}

	var bodyReader io.Reader
	if params.Body != "" {
		bodyReader = io.NopCloser(
			io.LimitReader(
				readerFromString(params.Body), int64(len(params.Body)),
			),
		)
	}

	req, err := http.NewRequestWithContext(ctx, params.Method, params.URL, bodyReader)
	if err != nil {
		return "", fmt.Errorf("http-call: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http-call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("http-call: reading response: %w", err)
	}

	result := struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}{
		Status: resp.StatusCode,
		Body:   string(respBody),
	}
	out, _ := json.Marshal(result)
	return string(out), nil
}

// readerFromString returns an io.Reader for the given string.
func readerFromString(s string) io.Reader {
	return io.LimitReader(
		&stringReader{s: s},
		int64(len(s)),
	)
}

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (n int, err error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n = copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}

// logHandler parses {"message": "..."}, logs it, and returns {"logged": true}.
func logHandler(_ context.Context, input string) (string, error) {
	var params struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return "", fmt.Errorf("log: invalid input: %w", err)
	}
	log.Printf("[activity:log] %s", params.Message)
	return `{"logged": true}`, nil
}

// failHandler always returns an error, useful for testing retries.
func failHandler(_ context.Context, _ string) (string, error) {
	return "", errors.New("activity failed")
}
