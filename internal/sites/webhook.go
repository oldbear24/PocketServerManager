package sites

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Push is a push to a Git branch, as a webhook reports it.
type Push struct {
	Branch  string
	Commit  string
	Message string
}

// Webhook errors. ErrNotAPush is for an event that is fine but asks for
// nothing, such as GitHub's ping or a branch that was deleted.
var (
	ErrSignature = errors.New("the signature is wrong or missing")
	ErrNotAPush  = errors.New("not a push")
)

// ParsePush checks a webhook's signature with the site's secret and reads its
// push. GitHub and Gitea or Forgejo sign the body (X-Hub-Signature-256,
// X-Gitea-Signature); GitLab sends the secret itself (X-Gitlab-Token).
func ParsePush(header http.Header, body []byte, secret string) (Push, error) {
	if secret == "" || !signed(header, body, secret) {
		return Push{}, ErrSignature
	}
	event := firstOf(header, "X-GitHub-Event", "X-Gitea-Event", "X-Forgejo-Event", "X-Gogs-Event", "X-Gitlab-Event")
	if event != "" && event != "push" && event != "Push Hook" {
		return Push{}, ErrNotAPush
	}

	var payload struct {
		Ref         string `json:"ref"`
		After       string `json:"after"`
		CheckoutSHA string `json:"checkout_sha"`
		HeadCommit  *struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"head_commit"`
		Commits []struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Push{}, errors.New("the body is not a push event's JSON")
	}
	branch, ok := strings.CutPrefix(payload.Ref, "refs/heads/")
	if !ok {
		// a tag
		return Push{}, ErrNotAPush
	}
	push := Push{Branch: branch, Commit: payload.After}
	if payload.CheckoutSHA != "" {
		push.Commit = payload.CheckoutSHA
	}
	if strings.Trim(push.Commit, "0") == "" {
		// the branch was deleted
		return Push{}, ErrNotAPush
	}
	if payload.HeadCommit != nil {
		push.Message = payload.HeadCommit.Message
	}
	for _, commit := range payload.Commits {
		if commit.ID == push.Commit {
			push.Message = commit.Message
		}
	}
	push.Message, _, _ = strings.Cut(strings.TrimSpace(push.Message), "\n")
	return push, nil
}

func signed(header http.Header, body []byte, secret string) bool {
	if token := header.Get("X-Gitlab-Token"); token != "" {
		return subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	want := mac.Sum(nil)
	for _, name := range []string{"X-Hub-Signature-256", "X-Gitea-Signature", "X-Forgejo-Signature", "X-Gogs-Signature"} {
		value := strings.TrimPrefix(header.Get(name), "sha256=")
		if value == "" {
			continue
		}
		got, err := hex.DecodeString(value)
		return err == nil && hmac.Equal(got, want)
	}
	return false
}

func firstOf(header http.Header, names ...string) string {
	for _, name := range names {
		if value := header.Get(name); value != "" {
			return value
		}
	}
	return ""
}
