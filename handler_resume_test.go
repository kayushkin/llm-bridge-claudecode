package main

import "testing"

// TestIsClaudeSessionUUID guards the resume/fork gate: only a canonical
// 8-4-4-4-12 hex UUID may reach `claude --resume`. Sub-agent rollout ids
// ("agent-<hex>") and bridge session ids must be rejected so the harness
// drops the flag instead of shelling out to a guaranteed-fail `--resume`.
func TestIsClaudeSessionUUID(t *testing.T) {
	valid := []string{
		"a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		"00000000-0000-0000-0000-000000000000",
		"A1B2C3D4-E5F6-7890-ABCD-EF1234567890", // uppercase hex
	}
	for _, s := range valid {
		if !isClaudeSessionUUID(s) {
			t.Errorf("isClaudeSessionUUID(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"",                                      // empty
		"agent-a245ee69c304b3ee9",               // sub-agent rollout id (the reported failure)
		"agent-a7b7f1f17a9fc339",                // another sub-agent id from the live DB
		"br_17840610686978557",                  // bridge session id
		"agent:claxon:main",                     // inber-style key
		"conformance-abc",                       // conformance harness id
		"a1b2c3d4-e5f6-7890-abcd-ef123456789",   // 35 chars (too short)
		"a1b2c3d4-e5f6-7890-abcd-ef12345678900", // 37 chars (too long)
		"a1b2c3d4e5f67890abcdef1234567890abcd",  // 36 chars but no dashes
		"a1b2c3d4-e5f6-7890-abcd-ef123456789g",  // non-hex digit 'g'
		"a1b2c3d4_e5f6_7890_abcd_ef1234567890",  // underscores, not dashes
		"za1b2c3d-e5f6-7890-abcd-ef1234567890",  // dash in the wrong slot
	}
	for _, s := range invalid {
		if isClaudeSessionUUID(s) {
			t.Errorf("isClaudeSessionUUID(%q) = true, want false", s)
		}
	}
}
