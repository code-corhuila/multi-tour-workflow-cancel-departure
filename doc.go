// Package workflow is the saga that cancels a departure and handles its reservations (ADR-004 D3, HU-WFL-004): operations-cost cancels
// the departure and commercial-reservation opens the rescheduling episode of each reservation. A cancellation cannot be undone, so the
// saga only goes forward and ends FAILED for a person when something keeps failing. The code lives under cmd/ and internal/.
package workflow
