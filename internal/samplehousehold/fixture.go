package samplehousehold

import _ "embed"

const Revision = "synthetic_household_v1"

// Fixture is the internal, noncustomer synthetic household shared by sample providers.
//
//go:embed synthetic-household-v1.json
var Fixture []byte
