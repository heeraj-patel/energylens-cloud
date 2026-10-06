package app

import "testing"

func TestSampleFixtureConfigGate(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "dev", APIListen: "127.0.0.1:0", DatabaseURL: "postgres://user:pass@localhost/db?sslmode=disable", SampleFixturesEnabled: true, SampleFixtureUserIDs: "11111111-1111-4111-8111-111111111111", SampleFixtureHomeIDs: "33333333-3333-4333-8333-333333333333"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid fixture config: %v", err)
	}
	for _, env := range []string{"production", "staging"} {
		c := base
		c.Environment = env
		if err := c.Validate(); err == nil {
			t.Fatalf("%s accepted fixture activation", env)
		}
	}
	c := base
	c.SampleFixtureUserIDs = ""
	if err := c.Validate(); err == nil {
		t.Fatal("fixture activation without allowlist")
	}
	c = base
	c.SampleFixtureHomeIDs = ""
	if err := c.Validate(); err == nil {
		t.Fatal("fixture activation without home allowlist")
	}
	c = base
	c.SampleFixturesEnabled = false
	if err := c.Validate(); err == nil {
		t.Fatal("inactive fixture config retained allowlist")
	}
	c = base
	c.Role = RoleWorker
	if err := c.Validate(); err == nil {
		t.Fatal("worker accepted fixture service")
	}
}

func TestSampleHouseholdConfigGate(t *testing.T) {
	base := Config{Role: RoleAPI, Environment: "test", APIListen: "127.0.0.1:0", DatabaseURL: "postgres://user:pass@localhost/db?sslmode=disable", SampleHouseholdEnabled: true, SampleHouseholdUserIDs: "11111111-1111-4111-8111-111111111111", SampleHouseholdHomeIDs: "33333333-3333-4333-8333-333333333333"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid sample household config: %v", err)
	}
	for _, env := range []string{"production", "staging"} {
		c := base
		c.Environment = env
		if err := c.Validate(); err == nil {
			t.Fatalf("%s accepted sample household", env)
		}
	}
	c := base
	c.SampleHouseholdUserIDs = ""
	if err := c.Validate(); err == nil {
		t.Fatal("missing user allowlist accepted")
	}
	c = base
	c.SampleHouseholdHomeIDs = ""
	if err := c.Validate(); err == nil {
		t.Fatal("missing home allowlist accepted")
	}
	c = base
	c.SampleHouseholdEnabled = false
	if err := c.Validate(); err == nil {
		t.Fatal("inactive gate retained allowlists")
	}
	c = base
	c.Role = RoleWorker
	if err := c.Validate(); err == nil {
		t.Fatal("worker accepted sample household")
	}
}
