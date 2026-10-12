# Interrupted upload admission fixture

`v0.7.5-after-intents.json` is immutable raw canonical authority written by
released commit `facf91f1ff64970c16717692619bfd2a93f5d1e9` (v0.7.5).
SHA-256: `c6b4d5d73a94073a6d761889b83cd56be46363de37157c24cb0cb50e2b300d84`.

The supplemental fixture interrupts the predecessor immediately after atomic
upload-intent publication, before provider sessions or transient leases exist.
It records two empty-file intents in a fresh split-backend storage set. It is
an identity/restart regression fixture, not a scale benchmark or a replacement
for the complete epoch/profile migration fixture matrix.

`producer_test.go` was copied into the predecessor's `internal/portable`
package and run with `nix develop -c go test ./internal/portable -run
'^TestProducePredecessorUploadIdentityFixture$' -count=1`. Production sources
came from `git archive` of the exact producer commit. The captured key/body map
was copied verbatim; current code did not normalize or repair it. The producer
uses the predecessor's existing deterministic split-backend fixture helper.

The current regression imports the raw bodies into fresh stores, proves that
the schema transition waits for capability drain, advances an injected clock,
then verifies unchanged historical IDs and explicit expired status after the
adjacent migration. It denies expired intent reactivation and admits fresh
independent keys at the same destinations. Never regenerate this fixture with
current code.
