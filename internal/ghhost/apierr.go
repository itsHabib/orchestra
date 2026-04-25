package ghhost

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// apiError captures the small subset of GitHub's error response that we care
// about. It is internal; callers inspect status via errors.Is(ErrBranchNotFound)
// or errors.As(*PullRequestExistsError).
type apiError struct {
	Status  int    `json:"-"`
	Message string `json:"message"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (e *apiError) Error() string {
	if len(e.Errors) > 0 {
		msgs := make([]string, 0, len(e.Errors))
		for _, sub := range e.Errors {
			msgs = append(msgs, sub.Message)
		}
		return fmt.Sprintf("github api %d: %s (%s)", e.Status, e.Message, strings.Join(msgs, "; "))
	}
	return fmt.Sprintf("github api %d: %s", e.Status, e.Message)
}

func parseAPIError(status int, data []byte) error {
	e := &apiError{Status: status}
	_ = json.Unmarshal(data, e)
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(data))
	}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	return e
}

func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func isAlreadyExists(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity {
		return false
	}
	if strings.Contains(strings.ToLower(ae.Message), "already exists") {
		return true
	}
	for _, sub := range ae.Errors {
		if strings.Contains(strings.ToLower(sub.Message), "already exists") {
			return true
		}
	}
	return false
}
