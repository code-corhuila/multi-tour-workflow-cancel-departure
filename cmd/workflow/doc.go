// Command workflow is the composition root of the saga that cancels a departure: it reads the configuration, builds the store, the participant
// clients, the orchestrator and the entry routes, and serves until it is told to stop.
package main
