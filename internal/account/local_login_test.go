package account

import (
	"context"
	"testing"
	"time"
)

func TestLocalLoginFailsClosedWithoutKeysAndStore(t *testing.T) {
	if _, err := (LocalLoginService{}).Login(context.Background(), "person@example.com", "correct long password", "192.0.2.1", time.Now()); err == nil {
		t.Fatal("unconfigured local login accepted")
	}
	if !VerifyLocalPassword(unknownAccountHash(), "unusable local account password") {
		t.Fatal("unknown-account path has no bounded Argon2id hash")
	}
}
