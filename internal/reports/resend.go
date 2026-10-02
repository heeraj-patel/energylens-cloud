package reports

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const resendEmailsURL = "https://api.resend.com/emails"

// ResendProvider uses Resend's 24-hour Idempotency-Key window. Report
// previews expire within 24 hours, so an approved request cannot be retried
// after the provider's duplicate-suppression window. This adapter is not
// installed or scheduled by the Cloud runtime.
// API: https://resend.com/docs/dashboard/emails/idempotency-keys
// Retention: https://resend.com/security/gdpr
type ResendProvider struct {
	apiKey   string
	from     string
	client   *http.Client
	endpoint string
}

func NewResendProvider(apiKey, from string) (*ResendProvider, error) {
	addresses, err := normalizeRecipients([]string{from})
	if strings.TrimSpace(apiKey) == "" || strings.ContainsAny(apiKey, "\r\n") || len(apiKey) > 512 || err != nil {
		return nil, errors.New("Resend API key and sender address required")
	}
	return &ResendProvider{apiKey: apiKey, from: addresses[0], endpoint: resendEmailsURL, client: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (p *ResendProvider) SendReport(ctx context.Context, requestID string, recipients []string, report RenderedReport) error {
	if p == nil || p.client == nil || p.apiKey == "" || p.endpoint == "" {
		return errors.New("report provider is not configured")
	}
	id, err := uuid.Parse(requestID)
	if err != nil || id == uuid.Nil || requestID != id.String() {
		return errors.New("canonical report request ID required")
	}
	to, err := normalizeRecipients(recipients)
	if err != nil || report.Subject != "EnergyLens report" || report.HTML == "" || len(report.HTML) > 512<<10 || len(report.SnapshotSHA256) != 64 {
		return errors.New("invalid rendered report request")
	}
	body, err := json.Marshal(struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		HTML    string   `json:"html"`
	}{p.from, to, report.Subject, report.HTML})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "report/"+requestID)
	response, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("report provider request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Provider responses may include recipients or body. Do not log them.
		return fmt.Errorf("report provider rejected request: HTTP %d", response.StatusCode)
	}
	var receipt struct {
		ID string `json:"id"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&receipt); err != nil || receipt.ID == "" || len(receipt.ID) > 128 {
		return errors.New("report provider returned an invalid receipt")
	}
	return nil
}
