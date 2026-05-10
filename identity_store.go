package main

import (
	"github.com/kayushkin/llm-bridge/identity"
	"github.com/oklog/ulid/v2"
)

// stateBackedStore satisfies identity.Store by bridging the package-level
// State (per-process state.db) to a per-session view. The bridgeSessionID
// is baked in; the Tracker only sees per-(harness_id, bridge_id) pairs.
type stateBackedStore struct {
	state           *State
	bridgeSessionID string
}

// Lookup implements identity.Store.
func (s *stateBackedStore) Lookup(harnessID string) (string, bool, error) {
	return s.state.LookupHarnessMessage(s.bridgeSessionID, harnessID)
}

// Put implements identity.Store.
func (s *stateBackedStore) Put(harnessID, messageID string) error {
	return s.state.PutHarnessMessage(s.bridgeSessionID, harnessID, messageID)
}

// newSessionTracker constructs an identity.Tracker for one session, backed
// by state.db. ULID minting is provided here so the identity package stays
// dependency-free.
func newSessionTracker(state *State, bridgeSessionID string) *identity.Tracker {
	store := &stateBackedStore{state: state, bridgeSessionID: bridgeSessionID}
	mint := func() string { return "msg_" + ulid.Make().String() }
	return identity.NewTracker(store, mint)
}
