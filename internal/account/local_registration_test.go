package account

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLocalRegistrationFailsClosedWithoutStoreSenderAndRateKey(t *testing.T) {
	service := LocalRegistrationService{}
	if _, err := service.RequestRegistration(context.Background(), "person@example.com", "correct long password", "192.0.2.1", time.Now()); err == nil {
		t.Fatal("unconfigured registration accepted")
	}
	if err := service.VerifyEmail(context.Background(), "bad", time.Now()); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("unconfigured verification accepted: %v", err)
	}
}
