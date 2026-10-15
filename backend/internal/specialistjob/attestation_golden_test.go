package specialistjob

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
)

// The cross-module result-attestation vector (E85.15 / #4363). The runner's
// upload.SpecialistJobResultMessage recomputes the SAME
// testdata/wire/specialist_job.json result_attestation vector
// (runner/internal/upload TestSpecialistJobResultMessage_MatchesGolden), so a
// divergence between the two modules' message constructions fails verify in
// whichever module drifted.
const sharedGoldenPath = "../../../testdata/wire/specialist_job.json"

type resultAttestationVector struct {
	SeedHex      string `json:"seed_hex"`
	PublicKeyHex string `json:"public_key_hex"`
	JobID        string `json:"job_id"`
	Body         string `json:"body"`
	MessageHex   string `json:"message_hex"`
	SignatureHex string `json:"signature_hex"`
}

func readSharedAttestationGolden(t *testing.T) (resultAttestationVector, string) {
	t.Helper()
	b, err := os.ReadFile(sharedGoldenPath)
	if err != nil {
		t.Fatalf("read shared golden: %v", err)
	}
	var doc struct {
		Vector   resultAttestationVector `json:"result_attestation"`
		Attested struct {
			AttestationKey string `json:"attestation_key"`
		} `json:"admission_attested"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode shared golden: %v", err)
	}
	if doc.Vector.SeedHex == "" || doc.Vector.MessageHex == "" || doc.Vector.SignatureHex == "" || doc.Attested.AttestationKey == "" {
		t.Fatalf("shared golden is missing result_attestation or admission_attested.attestation_key: %+v", doc)
	}
	return doc.Vector, doc.Attested.AttestationKey
}

// TestResultMessage_MatchesSharedGolden recomputes the shared vector with the
// backend ResultMessage: the digest, the deterministic signature, and its
// verification through VerifyResultAttestation.
func TestResultMessage_MatchesSharedGolden(t *testing.T) {
	v, _ := readSharedAttestationGolden(t)
	seed, err := hex.DecodeString(v.SeedHex)
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("seed_hex %q: %v", v.SeedHex, err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	if got := hex.EncodeToString(pub); got != v.PublicKeyHex {
		t.Fatalf("public key = %s, want the golden %s", got, v.PublicKeyHex)
	}
	jobID := uuid.MustParse(v.JobID)
	if jobID.String() != v.JobID {
		t.Fatalf("golden job_id %q is not the canonical form %q the server signs over", v.JobID, jobID.String())
	}
	msg := ResultMessage(jobID, []byte(v.Body))
	if got := hex.EncodeToString(msg); got != v.MessageHex {
		t.Fatalf("ResultMessage = %s, want the shared golden %s (the runner's twin pins the same vector)", got, v.MessageHex)
	}
	sig := ed25519.Sign(priv, msg)
	if got := hex.EncodeToString(sig); got != v.SignatureHex {
		t.Fatalf("signature = %s, want the shared golden %s", got, v.SignatureHex)
	}
	goldenSig, _ := hex.DecodeString(v.SignatureHex)
	stored := StoredAttestation{PublicKey: pub, Subject: "specialist-runner/golden", Live: true}
	if err := VerifyResultAttestation(stored, "specialist-runner/golden", jobID, []byte(v.Body), goldenSig); err != nil {
		t.Fatalf("golden signature refused: %v", err)
	}
}

// TestAdmissionAttestedKey_MatchesSharedGolden pins the admission's key
// encoding: attestation_key is base64 std of the 64-byte private key the
// vector's seed derives — the form the runner decodes.
func TestAdmissionAttestedKey_MatchesSharedGolden(t *testing.T) {
	v, key := readSharedAttestationGolden(t)
	seed, _ := hex.DecodeString(v.SeedHex)
	if want := base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed)); key != want {
		t.Fatalf("admission_attested.attestation_key = %s, want base64 std of the seed-derived key %s", key, want)
	}
}
