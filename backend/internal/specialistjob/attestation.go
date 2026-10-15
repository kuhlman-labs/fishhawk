package specialistjob

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Result attestation (E85.15 / #4363). A job admitted by a caller holding
// AttestScope gets a job-scoped Ed25519 key: the store keeps only the public
// half, bound to the job, the minting subject and an AttestationTTL sliding
// expiry decided on the database clock. The runner signs ResultMessage over
// the exact result body; a verified completion records
// ProvenanceRunnerAttested and consumes the key. Every other completion
// records ProvenanceBearer. BlockingEligible is the one predicate deciding
// ADR-089 D4 blocking eligibility. Contract: docs/spec/specialist-job-v1.md
// § "Result provenance and attestation".
const (
	// AttestScope is the dedicated token scope that obtains an attestation
	// key at admission. It is NOT in the operator default scope set (so the
	// HTTP token-mint ceiling refuses it) and NOT in the OAuth vocabulary;
	// it is minted only by `fishhawkd token issue --scopes`.
	AttestScope = "attest:specialist-jobs"
	// ProvenanceBearer: the result was shipped under a bearer token without a
	// verified runner signature. Advisory, never ADR-089 D4 blocking-grade.
	// A pre-0104 completion (provenance NULL) reads as bearer.
	ProvenanceBearer = "bearer"
	// ProvenanceRunnerAttested: the result body verified against the job's
	// stored attestation key, posted by the minting attester.
	ProvenanceRunnerAttested = "runner_attested"
	// AttestationTTL is the key's sliding expiry, refreshed only by the
	// minting attester's same-nonce heartbeats.
	AttestationTTL = 5 * time.Minute
	// ResultSignatureHeader carries the hex Ed25519 signature over
	// ResultMessage on POST /v0/specialist-jobs/{id}/result — the same header
	// ShipTrace and ShipAcceptance use.
	ResultSignatureHeader = "X-Fishhawk-Signature"
)

// resultMessagePrefix domain-separates a specialist-job result signature from
// every other Ed25519 message the product signs.
const resultMessagePrefix = "fishhawk-specialist-job-result-v1\x00"

// Attestation refusal reasons (AttestationError.Reason; the result route's
// 401 specialist_job_attestation_invalid details.reason).
const (
	ReasonNoAttestationKey   = "no_attestation_key"
	ReasonAttestationExpired = "attestation_expired"
	ReasonSubjectMismatch    = "subject_mismatch"
	ReasonSignatureMalformed = "signature_malformed"
	ReasonSignatureInvalid   = "signature_invalid"
	// ReasonCallerNotAttester is decided by the HTTP handler, not the store:
	// a signature header posted by a caller that is not an attester.
	ReasonCallerNotAttester = "caller_not_attester"
)

// ErrAttestationInvalid is a signed result the store refuses. Every refusal is
// an *AttestationError wrapping it; assert with errors.Is and read the reason
// with errors.As.
var ErrAttestationInvalid = errors.New("specialistjob: result attestation invalid")

// AttestationError is one attestation refusal with its reason.
type AttestationError struct {
	Reason string
}

func (e *AttestationError) Error() string {
	return fmt.Sprintf("%v: %s", ErrAttestationInvalid, e.Reason)
}

func (*AttestationError) Unwrap() error { return ErrAttestationInvalid }

func attestationRefused(reason string) error { return &AttestationError{Reason: reason} }

// ResultMessage is the digest the runner signs and the server verifies:
// sha256(prefix || jobID.String() || 0x00 || sha256(body)), where prefix is
// "fishhawk-specialist-job-result-v1\x00" and body is the EXACT result
// request body. The runner's upload.SpecialistJobResultMessage is its
// byte-identical twin.
func ResultMessage(jobID uuid.UUID, body []byte) []byte {
	inner := sha256.Sum256(body)
	h := sha256.New()
	h.Write([]byte(resultMessagePrefix))
	h.Write([]byte(jobID.String()))
	h.Write([]byte{0})
	h.Write(inner[:])
	return h.Sum(nil)
}

// BlockingEligible reports whether a result of this provenance may count as
// ADR-089 D4 blocking-grade evidence. It is the ONE such predicate: only a
// runner-attested result is; bearer, empty and unknown provenance are not.
func BlockingEligible(provenance string) bool {
	return provenance == ProvenanceRunnerAttested
}

// MintAttestationKey generates one job-scoped Ed25519 key pair from
// crypto/rand.
func MintAttestationKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("specialistjob: mint attestation key: %w", err)
	}
	return pub, priv, nil
}

// StoredAttestation is a running job's stored attestation state. Live is
// computed by SQL on the database clock (attestation_expires_at >
// clock_timestamp()), never on fishhawkd's clock.
type StoredAttestation struct {
	PublicKey ed25519.PublicKey
	Subject   string
	Live      bool
}

// VerifyResultAttestation checks a signed result against the job's stored
// attestation, in order: a key is present, it is live, the caller is the
// minting subject, the signature is well-formed, and it verifies over
// ResultMessage(jobID, body). Every refusal is an *AttestationError.
func VerifyResultAttestation(a StoredAttestation, callerSubject string, jobID uuid.UUID, body, sig []byte) error {
	// Load-bearing: ed25519.Verify panics on a key of the wrong length.
	if len(a.PublicKey) != ed25519.PublicKeySize {
		return attestationRefused(ReasonNoAttestationKey)
	}
	if !a.Live {
		return attestationRefused(ReasonAttestationExpired)
	}
	if callerSubject != a.Subject {
		return attestationRefused(ReasonSubjectMismatch)
	}
	if len(sig) != ed25519.SignatureSize {
		return attestationRefused(ReasonSignatureMalformed)
	}
	if !ed25519.Verify(a.PublicKey, ResultMessage(jobID, body), sig) {
		return attestationRefused(ReasonSignatureInvalid)
	}
	return nil
}
