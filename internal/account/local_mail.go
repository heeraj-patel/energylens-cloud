package account

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const localMailEndpoint = "https://api.resend.com/emails"

// ResendLocalSender is separate from worker-only report delivery credentials.
// The runtime does not install it until the local-auth activation gate.
type ResendLocalSender struct {
	apiKey, from, origin string
	endpoint             string
	client               *http.Client
}

func NewResendLocalSender(apiKey, from, origin string) (*ResendLocalSender, error) {
	if apiKey == "" || apiKey != strings.TrimSpace(apiKey) || len(apiKey) > 512 || strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("invalid auth mail API key")
	}
	normalized, err := NormalizeLocalEmail(from)
	if err != nil || normalized != from {
		return nil, errors.New("invalid auth mail sender")
	}
	u, err := url.Parse(origin)
	if err != nil || u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" || u.String() != origin || strings.HasSuffix(origin, "/") {
		return nil, errors.New("invalid auth link origin")
	}
	return &ResendLocalSender{apiKey: apiKey, from: from, origin: origin, endpoint: localMailEndpoint, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (s *ResendLocalSender) SendVerification(ctx context.Context, email, token string) error {
	return s.send(ctx, email, token, "verify_email")
}
func (s *ResendLocalSender) SendRecovery(ctx context.Context, email, token string) error {
	return s.send(ctx, email, token, "reset_password")
}

func (s *ResendLocalSender) send(ctx context.Context, email, token, purpose string) error {
	if s == nil || s.client == nil || s.endpoint != localMailEndpoint && !strings.HasPrefix(s.endpoint, "http://127.0.0.1:") {
		return errors.New("auth mail sender is not configured")
	}
	normalized, err := NormalizeLocalEmail(email)
	if err != nil || normalized != email {
		return errors.New("invalid auth mail recipient")
	}
	hash, err := HashLocalOpaqueToken(purpose, token)
	if err != nil {
		return ErrInvalidCredential
	}
	var path, subject, intro string
	if purpose == "verify_email" {
		path, subject, intro = "/verify-email", "Verify your EnergyLens email", "Use this one-hour link to verify your EnergyLens email:"
	} else if purpose == "reset_password" {
		path, subject, intro = "/reset-password", "Reset your EnergyLens password", "Use this one-hour link to reset your EnergyLens password:"
	} else {
		return ErrInvalidCredential
	}
	// Fragment data is not sent to the web origin in the HTTP request. The
	// eventual client must consume and clear it without third-party requests.
	link := s.origin + path + "#token=" + url.QueryEscape(token)
	body, err := json.Marshal(struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		Text    string   `json:"text"`
	}{s.from, []string{email}, subject, intro + "\n" + link + "\n\nIf you did not request this, you can ignore this message."})
	if err != nil {
		return errors.New("could not encode auth mail")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("could not create auth mail request")
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "auth/"+purpose+"/"+hex.EncodeToString(hash[:]))
	response, err := s.client.Do(req)
	if err != nil {
		return errors.New("auth mail transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("auth mail provider rejected request")
	}
	var receipt struct {
		ID string `json:"id"`
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &receipt) != nil || receipt.ID == "" || len(receipt.ID) > 128 {
		return errors.New("auth mail provider receipt invalid")
	}
	return nil
}
