package specialistjob

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// The pinned vector: a fixed job id and body, and a key from a fixed seed.
// Ed25519 signatures are deterministic (RFC 8032 §5.1.6), so the signature is
// frozen too. These literals were computed once and frozen: any change to the
// prefix, the separators or the inner hash reddens TestResultMessage_PinnedVector.
const (
	vectorJobID      = "11111111-2222-4333-8444-555555555555"
	vectorBody       = `{"schema_version":"specialist-job-v1","kind":"inventory","verdict":"pass"}`
	vectorMessageHex = "b6a8dcfb08c94269aadcf25a2ea3201d04ed5e77b85e575af57c3fb9c58fa209"
	vectorSigHex     = "5e95d75938311225782f0b052c92064f3bc55d860979622e2a0c392b748e4324b64503145123baf7d938cac2273855b007ec9fafb6d05371e216cdbfe499f10c"
)

func vectorKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestResultMessage_PinnedVector(t *testing.T) {
	msg := ResultMessage(uuid.MustParse(vectorJobID), []byte(vectorBody))
	if got := hex.EncodeToString(msg); got != vectorMessageHex {
		t.Fatalf("ResultMessage = %s, want the frozen %s", got, vectorMessageHex)
	}
	if got := hex.EncodeToString(ed25519.Sign(vectorKey(), msg)); got != vectorSigHex {
		t.Fatalf("signature = %s, want the frozen %s", got, vectorSigHex)
	}
}

// attested is a valid, live attestation by subject "specialist-runner/h1" and
// a body signed under it, so each refusal test changes exactly one input.
type attested struct {
	stored StoredAttestation
	priv   ed25519.PrivateKey
	jobID  uuid.UUID
	body   []byte
	sig    []byte
}

const attesterSubject = "specialist-runner/h1"

func newAttested(t *testing.T) attested {
	t.Helper()
	pub, priv, err := MintAttestationKey()
	if err != nil {
		t.Fatalf("MintAttestationKey: %v", err)
	}
	jobID := uuid.New()
	body := []byte(vectorBody)
	return attested{
		stored: StoredAttestation{PublicKey: pub, Subject: attesterSubject, Live: true},
		priv:   priv, jobID: jobID, body: body,
		sig: ed25519.Sign(priv, ResultMessage(jobID, body)),
	}
}

func (a attested) verify() error {
	return VerifyResultAttestation(a.stored, attesterSubject, a.jobID, a.body, a.sig)
}

// wantReason asserts err is an *AttestationError wrapping
// ErrAttestationInvalid with the given reason.
func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	if !errors.Is(err, ErrAttestationInvalid) {
		t.Fatalf("err = %v, want ErrAttestationInvalid", err)
	}
	var ae *AttestationError
	if !errors.As(err, &ae) || ae.Reason != reason {
		t.Fatalf("err = %v, want reason %s", err, reason)
	}
}

func TestVerifyResultAttestation_AcceptsValid(t *testing.T) {
	if err := newAttested(t).verify(); err != nil {
		t.Fatalf("valid attestation refused: %v", err)
	}
}

// With the length guard deleted, ed25519.Verify receives a nil key and
// panics, so this test goes RED by panic.
func TestVerifyResultAttestation_RefusesMissingKey(t *testing.T) {
	a := newAttested(t)
	a.stored.PublicKey = nil
	wantReason(t, a.verify(), ReasonNoAttestationKey)
	a.stored.PublicKey = ed25519.PublicKey(make([]byte, 31))
	wantReason(t, a.verify(), ReasonNoAttestationKey)
}

// Every other check passes on this fixture, so only the liveness check can
// refuse it.
func TestVerifyResultAttestation_RefusesExpiredKey(t *testing.T) {
	a := newAttested(t)
	a.stored.Live = false
	wantReason(t, a.verify(), ReasonAttestationExpired)
}

// The signature is genuine, so only the subject check can refuse it.
func TestVerifyResultAttestation_RefusesForeignSubject(t *testing.T) {
	a := newAttested(t)
	err := VerifyResultAttestation(a.stored, "specialist-runner/other", a.jobID, a.body, a.sig)
	wantReason(t, err, ReasonSubjectMismatch)
	err = VerifyResultAttestation(a.stored, "", a.jobID, a.body, a.sig)
	wantReason(t, err, ReasonSubjectMismatch)
}

func TestVerifyResultAttestation_RefusesMalformedSignature(t *testing.T) {
	a := newAttested(t)
	a.sig = a.sig[:10]
	wantReason(t, a.verify(), ReasonSignatureMalformed)
}

// The key, subject and liveness are valid, so verification is the only
// refusing check.
func TestVerifyResultAttestation_RefusesTamperedBody(t *testing.T) {
	a := newAttested(t)
	tampered := append([]byte(nil), a.body...)
	tampered[len(tampered)-3] ^= 0x01
	err := VerifyResultAttestation(a.stored, attesterSubject, a.jobID, tampered, a.sig)
	wantReason(t, err, ReasonSignatureInvalid)
}

func TestVerifyResultAttestation_RefusesOtherJobsKey(t *testing.T) {
	a, b := newAttested(t), newAttested(t)
	// A signature by job A's key over job B's message, checked against B's
	// stored key.
	sig := ed25519.Sign(a.priv, ResultMessage(b.jobID, b.body))
	err := VerifyResultAttestation(b.stored, attesterSubject, b.jobID, b.body, sig)
	wantReason(t, err, ReasonSignatureInvalid)
}

// Pins the job-id domain binding: the same key and body under another job id
// do not verify.
func TestVerifyResultAttestation_RefusesSameBodyOtherJobID(t *testing.T) {
	a := newAttested(t)
	err := VerifyResultAttestation(a.stored, attesterSubject, uuid.New(), a.body, a.sig)
	wantReason(t, err, ReasonSignatureInvalid)
}

func TestBlockingEligible_OnlyRunnerAttested(t *testing.T) {
	for provenance, want := range map[string]bool{
		ProvenanceRunnerAttested: true,
		ProvenanceBearer:         false,
		"":                       false,
		"runner_signed":          false,
	} {
		if got := BlockingEligible(provenance); got != want {
			t.Errorf("BlockingEligible(%q) = %v, want %v", provenance, got, want)
		}
	}
}

func TestAttestationError_MessageNamesReason(t *testing.T) {
	err := attestationRefused(ReasonSignatureInvalid)
	if got := err.Error(); got != "specialistjob: result attestation invalid: signature_invalid" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestMintAttestationKey_Sizes(t *testing.T) {
	pub, priv, err := MintAttestationKey()
	if err != nil {
		t.Fatalf("MintAttestationKey: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize || len(priv) != ed25519.PrivateKeySize {
		t.Fatalf("key sizes = %d/%d, want %d/%d", len(pub), len(priv), ed25519.PublicKeySize, ed25519.PrivateKeySize)
	}
}
