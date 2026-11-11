package saga

import "regexp"

var (
	// departureCode is the shape of the code of a departure. It is text a person typed and it goes into the paths of the participants, so
	// nothing else is accepted: not a slash, a dot-dot, a query or a space.
	departureCode = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,39}$`)
	// idShape is the shape of an id: the tenant, a saga, a departure and a reservation are ids, and they go into paths and into SQL.
	idShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// ValidDepartureCode says whether text may be the code of a departure.
func ValidDepartureCode(text string) bool { return departureCode.MatchString(text) }

// ValidID says whether text is an id (a UUID, in either case).
func ValidID(text string) bool { return idShape.MatchString(text) }
