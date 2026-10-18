package storageformat

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/applyinnovations/endlessfs/internal/domain"
)

func reviewWireProofFixture(t *testing.T) (DomainPagePack, []byte) {
	t.Helper()
	pack := testDomainPagePack(t)
	first := pack.Pages[0]
	second := DomainPage{SchemaVersion: 1, DomainID: pack.DomainID, Kind: pack.Kind, Entries: []DomainEntry{{Key: "beta", Value: []byte("independent second member <>&\u2028"), LogicalVersion: "version-b"}}}
	secondBody, err := EncodeCanonical(second)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest := Digest(secondBody)
	branch := DomainPage{SchemaVersion: 1, DomainID: pack.DomainID, Kind: pack.Kind, Level: 1, Children: []DomainPageChild{
		{FirstKey: "alpha", LastKey: "alpha", Digest: first.Digest, PackID: pack.PackID, EntryCount: 1, ByteCount: uint64(len(first.Page.Entries[0].Value)), LeafKeyFilter: DomainLeafKeyFilter([]string{"alpha"})},
		{FirstKey: "beta", LastKey: "beta", Digest: secondDigest, PackID: pack.PackID, EntryCount: 1, ByteCount: uint64(len(second.Entries[0].Value)), LeafKeyFilter: DomainLeafKeyFilter([]string{"beta"})},
	}}
	branchBody, err := EncodeCanonical(branch)
	if err != nil {
		t.Fatal(err)
	}
	pack.Pages = append(pack.Pages, DomainPackedPage{Digest: secondDigest, Page: second}, DomainPackedPage{Digest: Digest(branchBody), Page: branch})
	body, err := EncodeDomainPagePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	// The strict reader supplies canonical member order, independent of the
	// fixture's construction order.
	pack, err = DecodeDomainPagePack(context.Background(), body, pack.DomainID, pack.Kind, pack.PackID)
	if err != nil {
		t.Fatal(err)
	}
	return pack, body
}

func reviewIssueWireProof(t *testing.T, pack DomainPagePack, body []byte) (DomainPagePack, []byte, DomainPagePackWireProof) {
	t.Helper()
	decoded, owned, proof, err := DecodeDomainPagePackWithWireProof(context.Background(), body, pack.DomainID, pack.Kind, pack.PackID)
	if err != nil || !reflect.DeepEqual(decoded, pack) || !bytes.Equal(owned, body) {
		t.Fatalf("strict proof issuance did not preserve all canonical members: err=%v", err)
	}
	if len(owned) == 0 || &owned[0] == &body[0] {
		t.Fatal("strict proof issuance retained the provider's mutable buffer")
	}
	return decoded, owned, proof
}

func reviewMutateDecodedWirePack(pack *DomainPagePack) {
	for index := range pack.Pages {
		page := &pack.Pages[index].Page
		if len(page.Entries) != 0 {
			page.Entries[0].Value[0] ^= 0x40
			page.Entries[0].LogicalVersion = "caller-mutated-version"
		} else if len(page.Children) != 0 {
			page.Children[0].Digest = Digest([]byte("caller-mutated-child"))
		}
	}
}

func TestDomainPagePackWireProofReviewOwnsValidatedWireAndFreshValues(t *testing.T) {
	pack, input := reviewWireProofFixture(t)
	original := append([]byte(nil), input...)
	issued, owned, proof := reviewIssueWireProof(t, pack, input)
	for index := range input {
		input[index] ^= 0x40
	}
	reviewMutateDecodedWirePack(&issued)
	for iteration := range 2 {
		decoded, err := DecodeProvenDomainPagePack(context.Background(), owned, pack.DomainID, pack.Kind, pack.PackID, proof)
		if err != nil || !reflect.DeepEqual(decoded, pack) {
			t.Fatalf("proven decode %d inherited caller mutations: err=%v", iteration, err)
		}
		if err := ValidateDomainPagePack(decoded); err != nil {
			t.Fatalf("proven decode lost strict member semantics: %v", err)
		}
		canonical, err := EncodeDomainPagePack(decoded)
		if err != nil || !bytes.Equal(canonical, original) {
			t.Fatalf("proven decode changed exact canonical wire: err=%v", err)
		}
		reviewMutateDecodedWirePack(&decoded)
	}
	if !bytes.Equal(owned, original) {
		t.Fatal("decoded values or provider buffer mutated the owned verified wire")
	}
}

func TestDomainPagePackWireProofReviewRejectsChangedWireAndBinding(t *testing.T) {
	pack, body := reviewWireProofFixture(t)
	_, owned, proof := reviewIssueWireProof(t, pack, body)
	other := testDomainPagePack(t)
	other.Pages[0].Page.Entries[0].Value = []byte("different but otherwise valid immutable wire")
	pageBody, err := EncodeCanonical(other.Pages[0].Page)
	if err != nil {
		t.Fatal(err)
	}
	other.Pages[0].Digest = Digest(pageBody)
	otherBody, err := EncodeDomainPagePack(other)
	if err != nil {
		t.Fatal(err)
	}
	other, err = DecodeDomainPagePack(context.Background(), otherBody, other.DomainID, other.Kind, other.PackID)
	if err != nil {
		t.Fatal(err)
	}
	_, otherOwned, otherProof := reviewIssueWireProof(t, other, otherBody)
	if _, err := DecodeProvenDomainPagePack(context.Background(), otherOwned, other.DomainID, other.Kind, other.PackID, otherProof); err != nil {
		t.Fatalf("second valid proof positive control failed: %v", err)
	}
	corrupt := append([]byte(nil), owned...)
	corrupt[len(corrupt)/2] ^= 1
	alternateHeader := append([]byte(nil), owned...)
	alternateHeader[len(domainPagePackMagic)+4] ^= 1
	for _, candidate := range []struct {
		name, owner, packID string
		kind                ConsistencyDomainKind
		body                []byte
		proof               DomainPagePackWireProof
	}{
		{"zero-proof", pack.DomainID, pack.PackID, pack.Kind, owned, DomainPagePackWireProof{}},
		{"owner", "different-owner", pack.PackID, pack.Kind, owned, proof},
		{"kind", pack.DomainID, pack.PackID, DomainShare, owned, proof},
		{"pack", pack.DomainID, Digest([]byte("different-pack")), pack.Kind, owned, proof},
		{"corrupt-same-length", pack.DomainID, pack.PackID, pack.Kind, corrupt, proof},
		{"valid-different-wire-same-binding", pack.DomainID, pack.PackID, pack.Kind, otherOwned, proof},
		{"different-valid-proof", pack.DomainID, pack.PackID, pack.Kind, owned, otherProof},
		{"alternate-gzip-header", pack.DomainID, pack.PackID, pack.Kind, alternateHeader, proof},
		{"truncated", pack.DomainID, pack.PackID, pack.Kind, owned[:len(owned)-1], proof},
		{"empty", pack.DomainID, pack.PackID, pack.Kind, nil, proof},
		{"wire-bound", pack.DomainID, pack.PackID, pack.Kind, make([]byte, MaxDomainPagePackBytes+1), proof},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			decoded, err := DecodeProvenDomainPagePack(context.Background(), candidate.body, candidate.owner, candidate.kind, candidate.packID, candidate.proof)
			if !errors.Is(err, domain.ErrInvalid) || !reflect.DeepEqual(decoded, DomainPagePack{}) {
				t.Fatalf("unproven wire escaped as decoded authority: err=%v", err)
			}
		})
	}
}

func TestDomainPagePackWireProofReviewIssuanceRejectsUnverifiedMembersAndEncoding(t *testing.T) {
	pack, body := reviewWireProofFixture(t)
	payload, err := EncodeCanonical(pack)
	if err != nil {
		t.Fatal(err)
	}
	// Only the second leaf is invalid; a reader interested in alpha must not
	// obtain a whole-pack proof without authenticating this unused member.
	invalid := pack
	invalid.Pages = append([]DomainPackedPage(nil), pack.Pages...)
	for index := range invalid.Pages {
		page := &invalid.Pages[index].Page
		if len(page.Entries) > 0 && page.Entries[0].Key == "beta" {
			page.SchemaVersion = 0
		}
	}
	invalidPayload, err := EncodeCanonical(invalid)
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(payload, []byte(`"schemaVersion":1`), []byte(`"unexpected":true,"schemaVersion":1`), 1)
	duplicate := bytes.Replace(payload, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"schemaVersion":1`), 1)
	for name, candidate := range map[string][]byte{
		"invalid-unused-member": encodeDomainPackPayload(t, invalidPayload),
		"unknown-json-field":    encodeDomainPackPayload(t, unknown),
		"duplicate-json-field":  encodeDomainPackPayload(t, duplicate),
		"noncanonical-space":    encodeDomainPackPayload(t, append([]byte(" "), payload...)),
		"trailing-json":         encodeDomainPackPayload(t, append(append([]byte(nil), payload...), []byte("null")...)),
		"bad-gzip-footer":       body[:len(body)-1],
		"expanded-bound":        encodeDomainPackPayload(t, bytes.Repeat([]byte("x"), MaxExpandedDomainPagePackBytes+1)),
		"wire-bound":            make([]byte, MaxDomainPagePackBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeDomainPagePack(context.Background(), candidate, pack.DomainID, pack.Kind, pack.PackID); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("strict negative control accepted malformed wire: %v", err)
			}
			decoded, owned, proof, err := DecodeDomainPagePackWithWireProof(context.Background(), candidate, pack.DomainID, pack.Kind, pack.PackID)
			if !errors.Is(err, domain.ErrInvalid) || len(owned) != 0 || !reflect.DeepEqual(decoded, DomainPagePack{}) || !reflect.DeepEqual(proof, DomainPagePackWireProof{}) {
				t.Fatalf("failed strict validation issued owned authority or a proof: err=%v", err)
			}
			if _, err := DecodeProvenDomainPagePack(context.Background(), body, pack.DomainID, pack.Kind, pack.PackID, proof); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("failed strict validation produced a usable proof: %v", err)
			}
		})
	}
	for _, binding := range []struct {
		name, owner, packID string
		kind                ConsistencyDomainKind
	}{{"owner", "another-owner", pack.PackID, pack.Kind}, {"kind", pack.DomainID, pack.PackID, DomainShare}, {"pack", pack.DomainID, Digest([]byte("another-pack")), pack.Kind}} {
		t.Run("binding-"+binding.name, func(t *testing.T) {
			_, owned, proof, err := DecodeDomainPagePackWithWireProof(context.Background(), body, binding.owner, binding.kind, binding.packID)
			if !errors.Is(err, domain.ErrInvalid) || len(owned) != 0 || !reflect.DeepEqual(proof, DomainPagePackWireProof{}) {
				t.Fatalf("wrong binding issued a proof: %v", err)
			}
		})
	}
}

type reviewWireProofWaitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *reviewWireProofWaitingContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestDomainPagePackWireProofReviewDoesNotAllocateWireCopyWhileQueued(t *testing.T) {
	pack, body := reviewWireProofFixture(t)
	reviewIssueWireProof(t, pack, body)
	// This maximum-sized provider buffer is never admitted or decoded: the
	// caller is canceled while the real global decoder slot remains held.
	// Allocate it before accounting so the test measures only queued work.
	input := make([]byte, MaxDomainPagePackBytes)
	copy(input, domainPagePackMagic)
	domainPagePackDecodeSlot <- struct{}{}
	defer func() { <-domainPagePackDecodeSlot }()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &reviewWireProofWaitingContext{Context: parent, waiting: make(chan struct{})}
	finished := make(chan error, 1)
	var before, blocked runtime.MemStats
	runtime.ReadMemStats(&before)
	go func() {
		_, owned, proof, err := DecodeDomainPagePackWithWireProof(ctx, input, pack.DomainID, pack.Kind, pack.PackID)
		if len(owned) != 0 || !reflect.DeepEqual(proof, DomainPagePackWireProof{}) {
			finished <- errors.New("canceled queued issuer exposed owned wire or a usable proof")
			return
		}
		finished <- err
	}()
	select {
	case <-ctx.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("issuer did not reach the shared decoder admission wait")
	}
	runtime.ReadMemStats(&blocked)
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) {
			t.Fatalf("queued issuer failed to preserve cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued issuer ignored cancellation while decoder admission was blocked")
	}
	// A single forbidden compressed-wire clone alone allocates this many
	// bytes. The strict inequality tolerates ordinary goroutine/context
	// bookkeeping while ruling out even one such copy before admission.
	allocated := blocked.TotalAlloc - before.TotalAlloc
	t.Logf("queued issuer allocated=%d bytes; one compressed-wire copy=%d bytes", allocated, len(input))
	if allocated >= uint64(len(input)) {
		t.Fatalf("queued issuer allocated at least one compressed-wire copy: allocated=%d wire=%d", allocated, len(input))
	}
}

type reviewWireProofCountingContext struct {
	context.Context
	checks      int
	allowChecks int
	cancel      context.CancelFunc
}

func (ctx *reviewWireProofCountingContext) Err() error {
	ctx.checks++
	if ctx.cancel != nil && ctx.checks > ctx.allowChecks {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestDomainPagePackWireProofReviewCancellationBeforeIssuanceReturnsNoProof(t *testing.T) {
	pack, body := reviewWireProofFixture(t)
	strict := &reviewWireProofCountingContext{Context: context.Background()}
	if _, err := DecodeDomainPagePack(strict, body, pack.DomainID, pack.Kind, pack.PackID); err != nil {
		t.Fatalf("strict positive control failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Allow every check needed by the complete strict decode of this exact
	// fixture. Cancel at the proof wrapper's next check, after strict
	// validation and before issuance.
	// Measuring the strict count avoids hard-coding gzip reader iterations.
	boundary := &reviewWireProofCountingContext{Context: ctx, allowChecks: strict.checks, cancel: cancel}
	decoded, owned, proof, err := DecodeDomainPagePackWithWireProof(boundary, body, pack.DomainID, pack.Kind, pack.PackID)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) || len(owned) != 0 || !reflect.DeepEqual(decoded, DomainPagePack{}) || !reflect.DeepEqual(proof, DomainPagePackWireProof{}) {
		t.Fatalf("cancellation at issuance exposed validated authority or proof: err=%v", err)
	}
	if boundary.checks != boundary.allowChecks+1 {
		t.Fatal("cancellation did not occur at the post-validation issuance boundary")
	}
}

func TestDomainPagePackWireProofReviewCancellationAndSlotWaiting(t *testing.T) {
	pack, body := reviewWireProofFixture(t)
	_, owned, proof := reviewIssueWireProof(t, pack, body)
	for _, decoder := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"strict", func(ctx context.Context) error {
			_, err := DecodeDomainPagePack(ctx, body, pack.DomainID, pack.Kind, pack.PackID)
			return err
		}},
		{"issue", func(ctx context.Context) error {
			_, canceledOwned, canceledProof, err := DecodeDomainPagePackWithWireProof(ctx, body, pack.DomainID, pack.Kind, pack.PackID)
			if len(canceledOwned) != 0 || !reflect.DeepEqual(canceledProof, DomainPagePackWireProof{}) {
				return errors.New("cancellation issued a usable proof or owned wire")
			}
			return err
		}},
		{"proven", func(ctx context.Context) error {
			_, err := DecodeProvenDomainPagePack(ctx, owned, pack.DomainID, pack.Kind, pack.PackID, proof)
			return err
		}},
	} {
		t.Run(decoder.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := decoder.run(ctx); !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("initial cancellation lost its classification: %v", err)
			}
			deadline, cancelDeadline := context.WithDeadline(context.Background(), time.Unix(0, 0))
			defer cancelDeadline()
			if err := decoder.run(deadline); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("expired deadline lost its classification: %v", err)
			}
			// Hold the actual global slot. Observing Done evaluation proves the
			// decoder reached its cancellable wait, without scheduling sleeps.
			domainPagePackDecodeSlot <- struct{}{}
			defer func() { <-domainPagePackDecodeSlot }()
			waitingCtx, cancelWaiting := context.WithCancel(context.Background())
			defer cancelWaiting()
			observed := &reviewWireProofWaitingContext{Context: waitingCtx, waiting: make(chan struct{})}
			finished := make(chan error, 1)
			go func() { finished <- decoder.run(observed) }()
			select {
			case <-observed.waiting:
			case <-time.After(2 * time.Second):
				t.Fatal("decoder did not reach the shared slot wait")
			}
			cancelWaiting()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrUnavailable) {
					t.Fatalf("waiting cancellation lost its classification: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("decoder ignored cancellation while waiting for the shared slot")
			}
			if len(domainPagePackDecodeSlot) != 1 {
				t.Fatal("canceled decoder consumed or released another caller's slot")
			}
		})
	}
	if decoded, err := DecodeProvenDomainPagePack(context.Background(), owned, pack.DomainID, pack.Kind, pack.PackID, proof); err != nil || !reflect.DeepEqual(decoded, pack) {
		t.Fatalf("independent cancellation invalidated good proof: %v", err)
	}
}

func TestDomainPagePackWireProofReviewConcurrentDecodesHaveIndependentValues(t *testing.T) {
	pack, body := reviewWireProofFixture(t)
	_, owned, proof := reviewIssueWireProof(t, pack, body)
	const callers = 8
	decoded := make([]DomainPagePack, callers)
	errorsByCaller := make([]error, callers)
	var workers sync.WaitGroup
	for index := range callers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			decoded[index], errorsByCaller[index] = DecodeProvenDomainPagePack(context.Background(), owned, pack.DomainID, pack.Kind, pack.PackID, proof)
		}()
	}
	workers.Wait()
	for index := range callers {
		if errorsByCaller[index] != nil || !reflect.DeepEqual(decoded[index], pack) {
			t.Fatalf("caller %d inherited another decode's mutation or failed: %v", index, errorsByCaller[index])
		}
		reviewMutateDecodedWirePack(&decoded[index])
	}
	if final, err := DecodeProvenDomainPagePack(context.Background(), owned, pack.DomainID, pack.Kind, pack.PackID, proof); err != nil || !reflect.DeepEqual(final, pack) {
		t.Fatalf("concurrent callers mutated retained proof authority: %v", err)
	}
}
