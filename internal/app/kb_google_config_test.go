package app

import (
	"strings"
	"testing"
)

func TestKBGoogleSearchRequiresEPAConfirmationSiteAndCredentials(t *testing.T) {
	base := Config{Role: RoleWorker, Environment: "dev", APIListen: "127.0.0.1:0", DatabaseURL: "postgres://localhost/test", KBScanPython: "/usr/bin/python3", KBScanSources: "/tmp"}
	if err := base.Validate(); err != nil {
		t.Fatalf("offline default rejected: %v", err)
	}
	c := base
	c.KBScanGoogleSearch = true
	c.KBScanGoogleSite = "lg.com"
	c.KBScanGoogleExistingCustomer = true
	c.KBScanAllowEPA = true
	c.KBScanGoogleCSEAPIKey = "secret-key"
	c.KBScanGoogleCSEID = "engine-id"
	if err := c.Validate(); err != nil {
		t.Fatalf("complete Google search config rejected: %v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"EPA approval":                   func(c *Config) { c.KBScanAllowEPA = false },
		"existing customer confirmation": func(c *Config) { c.KBScanGoogleExistingCustomer = false },
		"public site":                    func(c *Config) { c.KBScanGoogleSite = "" },
		"CSE API key":                    func(c *Config) { c.KBScanGoogleCSEAPIKey = "" },
		"CSE ID":                         func(c *Config) { c.KBScanGoogleCSEID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := c
			mutate(&invalid)
			err := invalid.Validate()
			if err == nil || strings.Contains(err.Error(), "secret-key") {
				t.Fatalf("incomplete configuration accepted or secret leaked: %v", err)
			}
		})
	}
}

func TestLoadConfigParsesKBGoogleOptIn(t *testing.T) {
	for key, value := range map[string]string{
		"ENERGYLENS_CLOUD_ENV":                         "dev",
		"ENERGYLENS_CLOUD_BIND":                        "127.0.0.1:0",
		"ENERGYLENS_CLOUD_DATABASE_URL":                "postgres://localhost/test",
		"ENERGYLENS_CLOUD_KB_PYTHON":                   "/usr/bin/python3",
		"ENERGYLENS_CLOUD_KB_SOURCES":                  "/tmp",
		"ENERGYLENS_CLOUD_KB_GOOGLE_SEARCH":            "true",
		"ENERGYLENS_CLOUD_KB_GOOGLE_SITE":              "example.org",
		"ENERGYLENS_CLOUD_KB_GOOGLE_EXISTING_CUSTOMER": "true",
		"ENERGYLENS_GOOGLE_CSE_API_KEY":                "test-key",
		"ENERGYLENS_GOOGLE_CSE_ID":                     "test-engine",
		"ENERGYLENS_CLOUD_KB_ALLOW_EPA":                "true",
	} {
		t.Setenv(key, value)
	}
	c, err := LoadConfig("worker")
	if err != nil {
		t.Fatalf("load complete configuration: %v", err)
	}
	if !c.KBScanGoogleSearch || !c.KBScanGoogleExistingCustomer || !c.KBScanAllowEPA || c.KBScanGoogleSite != "example.org" || c.KBScanGoogleCSEAPIKey != "test-key" || c.KBScanGoogleCSEID != "test-engine" {
		t.Fatal("Google search configuration was not loaded")
	}
}
