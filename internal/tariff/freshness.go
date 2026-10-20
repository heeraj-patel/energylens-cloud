package tariff

import (
	"fmt"
	"time"
)

// ElectricityMaxAge is how long after its source_as_of an electricity tariff
// counts as current; older ones are treated as no tariff.
const ElectricityMaxAge = 90 * 24 * time.Hour

// ElectricityMaxAgeInterval is ElectricityMaxAge as a SQL interval literal.
var ElectricityMaxAgeInterval = fmt.Sprintf("interval '%d hours'", int(ElectricityMaxAge/time.Hour))
