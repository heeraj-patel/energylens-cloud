package app

import "testing"

func TestLocalAuthMailConfigIsSeparateAndDefaultsOff(t *testing.T) {
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_MAIL_ENABLED", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_API_KEY", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_RESEND_FROM", "")
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_LINK_ORIGIN", "")
	c, err := LoadConfig("api")
	if err != nil || c.LocalAuthMailEnabled {
		t.Fatalf("auth mail should default off: %+v %v", c, err)
	}
	base := Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0", DatabaseURL: "postgres://user:pass@localhost:5432/db?sslmode=disable", LocalAuthMailEnabled: true, LocalAuthResendAPIKey: "auth-test-key", LocalAuthResendFrom: "auth@example.test", LocalAuthLinkOrigin: "https://account.example.test"}
	if err = base.Validate(); err != nil {
		t.Fatalf("valid isolated auth mail config: %v", err)
	}
	if sender, buildErr := base.BuildLocalAuthSender(); buildErr != nil || sender == nil {
		t.Fatalf("enabled auth mail sender unavailable: %v", buildErr)
	}
	disabled := base
	disabled.LocalAuthMailEnabled = false
	if sender, buildErr := disabled.BuildLocalAuthSender(); buildErr != nil || sender != nil {
		t.Fatalf("disabled auth mail built a provider: %v", buildErr)
	}
	worker := base
	worker.Role = RoleWorker
	if err = worker.Validate(); err == nil {
		t.Fatal("worker accepted auth mail credentials")
	}
	if sender, buildErr := worker.BuildLocalAuthSender(); buildErr == nil || sender != nil {
		t.Fatal("worker built auth mail sender")
	}
	partial := base
	partial.LocalAuthResendFrom = ""
	if err = partial.Validate(); err == nil {
		t.Fatal("partial auth mail config accepted")
	}
	badOrigin := base
	badOrigin.LocalAuthLinkOrigin = "http://account.example.test"
	if err = badOrigin.Validate(); err == nil {
		t.Fatal("non-HTTPS auth link origin accepted")
	}
	withReport := base
	withReport.ResendAPIKey = "report-key"
	withReport.ResendFrom = "reports@example.test"
	if err = withReport.Validate(); err == nil {
		t.Fatal("API role accepted worker report credentials")
	}
	t.Setenv("ENERGYLENS_CLOUD_LOCAL_AUTH_MAIL_ENABLED", "maybe")
	if _, err = LoadConfig("api"); err == nil {
		t.Fatal("invalid auth mail flag accepted")
	}
}
