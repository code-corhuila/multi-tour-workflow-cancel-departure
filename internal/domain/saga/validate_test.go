package saga_test

import (
	"strings"
	"testing"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

// The code of a departure and the ids go into the paths of the participants and into SQL: one rule says what is acceptable, here, and
// the adapters ask it instead of each writing its own.
func TestD_a_departure_code_is_plain_and_at_most_forty_characters(t *testing.T) {
	for _, ok := range []string{"SAL-013", "a", "A1", "x.y_z-9", strings.Repeat("A", 40)} {
		if !saga.ValidDepartureCode(ok) {
			t.Errorf("%q should be a departure code", ok)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", "x?y=1", "x#z", "x y", "%2e%2e", ".hidden", "-x", "é", strings.Repeat("A", 41), "x\n"} {
		if saga.ValidDepartureCode(bad) {
			t.Errorf("%q should not be a departure code", bad)
		}
	}
}

func TestD_an_id_is_a_uuid_in_either_case_and_nothing_else(t *testing.T) {
	for _, ok := range []string{"11111111-1111-4111-8111-111111111111", "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"} {
		if !saga.ValidID(ok) {
			t.Errorf("%q should be an id", ok)
		}
	}
	for _, bad := range []string{"", "not-an-id", "..", "a/b", "11111111-1111-4111-8111-11111111111", "11111111-1111-4111-8111-1111111111111", "11111111-1111-4111-8111-11111111111g", "11111111-1111-4111-8111-111111111111\n"} {
		if saga.ValidID(bad) {
			t.Errorf("%q should not be an id", bad)
		}
	}
}

// The answer of the saga names the reservations that are not handled: the ones that failed and the ones still pending, never the opened.
func TestD_left_names_the_reservations_that_are_not_opened(t *testing.T) {
	s := saga.Saga{Reservations: []saga.Reservation{{ID: "a", State: saga.Opened}, {ID: "b", State: saga.EpisodeFailed}, {ID: "c", State: saga.Pending}}}
	if left := s.Left(); len(left) != 2 || left[0] != "b" || left[1] != "c" {
		t.Fatalf("left %v, want b and c", left)
	}
	if s.Handled() != 1 {
		t.Fatalf("handled %d, want 1", s.Handled())
	}
}
