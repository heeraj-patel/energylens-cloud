package energyrate

// Offline U.S. ZIP-prefix → state table.
//
// Source: the USPS three-digit ZIP Code prefix assignments (USPS Publication
// 65 / the "3-digit ZIP Code prefix" sectional-center allocations, as
// transcribed in Wikipedia's "List of ZIP Code prefixes"). The first three
// digits of a ZIP identify its sectional center facility, and every facility
// lies in exactly one state, so a prefix range table is enough to resolve a
// ZIP to its state without any network lookup and without storing ZIPs.
//
// Deliberately resolved to "" (unknown → national average):
//   - prefixes outside every range below (e.g. 000-004, 568, 578-579, 589,
//     694-699, 715, 732 — unassigned, so no real ZIP starts with them;
//     unassigned prefixes *inside* a state's run are simply left in that
//     run, since no real ZIP uses them either);
//   - U.S. territories (PR 006-007/009, VI 008, GU/MP/AS/FM/MH/PW 969) — the
//     EIA retail-sales series covers the 50 states + DC only;
//   - military APO/FPO/DPO prefixes (090-099 AE, 340 AA, 962-966 AP).
//
// A handful of single prefixes sit inside another state's range (IRS and
// parcel-return facilities): 005 (Holtsville) and 055 (Andover) are NY/MA,
// 569 is DC, 733 (Austin IRS) and 885 (El Paso) are TX; they are listed
// explicitly below. Coarse by design — an energy-rate average only needs the
// state, and a wrong state for a border ZIP is a small rate error, not a
// correctness bug.

type zipRange struct {
	lo, hi int // inclusive 3-digit prefix bounds
	state  string
}

// zipRanges is sorted by lo and non-overlapping (checked by a unit test).
var zipRanges = []zipRange{
	{5, 5, "NY"},
	{10, 27, "MA"},
	{28, 29, "RI"},
	{30, 38, "NH"},
	{39, 49, "ME"},
	{50, 54, "VT"},
	{55, 55, "MA"},
	{56, 59, "VT"},
	{60, 69, "CT"},
	{70, 89, "NJ"},
	{100, 149, "NY"},
	{150, 196, "PA"},
	{197, 199, "DE"},
	{200, 200, "DC"},
	{201, 201, "VA"},
	{202, 205, "DC"},
	{206, 219, "MD"},
	{220, 246, "VA"},
	{247, 268, "WV"},
	{270, 289, "NC"},
	{290, 299, "SC"},
	{300, 319, "GA"},
	{320, 339, "FL"},
	{341, 349, "FL"},
	{350, 369, "AL"},
	{370, 385, "TN"},
	{386, 397, "MS"},
	{398, 399, "GA"},
	{400, 429, "KY"},
	{430, 459, "OH"},
	{460, 479, "IN"},
	{480, 499, "MI"},
	{500, 529, "IA"},
	{530, 549, "WI"},
	{550, 567, "MN"},
	{569, 569, "DC"},
	{570, 577, "SD"},
	{580, 588, "ND"},
	{590, 599, "MT"},
	{600, 629, "IL"},
	{630, 658, "MO"},
	{660, 679, "KS"},
	{680, 693, "NE"},
	{700, 714, "LA"},
	{716, 729, "AR"},
	{730, 731, "OK"},
	{733, 733, "TX"},
	{734, 749, "OK"},
	{750, 799, "TX"},
	{800, 816, "CO"},
	{820, 831, "WY"},
	{832, 838, "ID"},
	{840, 847, "UT"},
	{850, 865, "AZ"},
	{870, 884, "NM"},
	{885, 885, "TX"},
	{889, 898, "NV"},
	{900, 961, "CA"},
	{967, 968, "HI"},
	{970, 979, "OR"},
	{980, 994, "WA"},
	{995, 999, "AK"},
}

// stateNames maps every state code zipRanges can return (50 states + DC) to
// the display name used in source_label.
var stateNames = map[string]string{
	"AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas",
	"CA": "California", "CO": "Colorado", "CT": "Connecticut", "DE": "Delaware",
	"DC": "District of Columbia", "FL": "Florida", "GA": "Georgia", "HI": "Hawaii",
	"ID": "Idaho", "IL": "Illinois", "IN": "Indiana", "IA": "Iowa",
	"KS": "Kansas", "KY": "Kentucky", "LA": "Louisiana", "ME": "Maine",
	"MD": "Maryland", "MA": "Massachusetts", "MI": "Michigan", "MN": "Minnesota",
	"MS": "Mississippi", "MO": "Missouri", "MT": "Montana", "NE": "Nebraska",
	"NV": "Nevada", "NH": "New Hampshire", "NJ": "New Jersey", "NM": "New Mexico",
	"NY": "New York", "NC": "North Carolina", "ND": "North Dakota", "OH": "Ohio",
	"OK": "Oklahoma", "OR": "Oregon", "PA": "Pennsylvania", "RI": "Rhode Island",
	"SC": "South Carolina", "SD": "South Dakota", "TN": "Tennessee", "TX": "Texas",
	"UT": "Utah", "VT": "Vermont", "VA": "Virginia", "WA": "Washington",
	"WV": "West Virginia", "WI": "Wisconsin", "WY": "Wyoming",
}

// StateForZIP returns the two-letter state code for a 5-digit U.S. ZIP, or ""
// when the prefix is unassigned, a territory, or military. zip must already be
// normalized to exactly five ASCII digits.
func StateForZIP(zip string) string {
	if len(zip) < 3 {
		return ""
	}
	p := 0
	for i := 0; i < 3; i++ {
		c := zip[i]
		if c < '0' || c > '9' {
			return ""
		}
		p = p*10 + int(c-'0')
	}
	// Binary search over the sorted ranges.
	lo, hi := 0, len(zipRanges)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		r := zipRanges[mid]
		switch {
		case p < r.lo:
			hi = mid - 1
		case p > r.hi:
			lo = mid + 1
		default:
			return r.state
		}
	}
	return ""
}
