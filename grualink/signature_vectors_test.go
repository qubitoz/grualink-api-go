package grualink

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

type vectorCase struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Secret      string            `json:"secret"`
	Headers     map[string]string `json:"headers"`
	Body        *string           `json:"body"`
	BodyBase64  string            `json:"body_base64"`
	Now         int64             `json:"now"`
	Expected    struct {
		Valid  bool   `json:"valid"`
		Reason string `json:"reason"`
	} `json:"expected"`
}

type vectorFile struct {
	Version          int          `json:"version"`
	ToleranceSeconds int64        `json:"tolerance_seconds"`
	Reasons          []string     `json:"reasons"`
	Cases            []vectorCase `json:"cases"`
}

// The contract kit stays at the repository root; tests run in the package folder.
func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	data, err := os.ReadFile("../contract/signature-vectors.json")
	if err != nil {
		t.Fatalf("cannot read the contract vectors: %v", err)
	}
	var vectors vectorFile
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("cannot parse the contract vectors: %v", err)
	}
	return vectors
}

func (c vectorCase) rawBody(t *testing.T) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(c.BodyBase64)
	if err != nil {
		t.Fatalf("%s: body_base64 is not base64: %v", c.Name, err)
	}
	return raw
}

func findVector(t *testing.T, vectors vectorFile, name string) vectorCase {
	t.Helper()
	for _, c := range vectors.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the contract vectors have no case named %s", name)
	return vectorCase{}
}

func TestTheVectorFileHasTheExpectedShape(t *testing.T) {
	vectors := loadVectors(t)
	if vectors.Version != 1 || vectors.ToleranceSeconds != 300 {
		t.Fatalf("version=%d tolerance=%d", vectors.Version, vectors.ToleranceSeconds)
	}
	if len(vectors.Cases) < 30 {
		t.Fatalf("expected at least 30 cases, got %d", len(vectors.Cases))
	}
	names := map[string]bool{}
	covered := map[string]bool{}
	valid, invalid := 0, 0
	for _, c := range vectors.Cases {
		if names[c.Name] {
			t.Errorf("duplicate case name %s", c.Name)
		}
		names[c.Name] = true
		covered[c.Expected.Reason] = true
		if c.Expected.Valid {
			valid++
		} else {
			invalid++
		}
	}
	if valid == 0 || invalid == 0 {
		t.Errorf("valid=%d invalid=%d", valid, invalid)
	}
	want := []SignatureVerificationReason{
		ReasonOK, ReasonMissingSecret, ReasonMissingHeader, ReasonMalformedHeader, ReasonTimestampOutOfTolerance,
		ReasonSignatureMismatch,
	}
	if len(vectors.Reasons) != len(want) {
		t.Fatalf("reasons = %v", vectors.Reasons)
	}
	for i, reason := range want {
		if vectors.Reasons[i] != string(reason) {
			t.Errorf("reason %d is %q in the contract and %q here", i, vectors.Reasons[i], reason)
		}
		if !covered[string(reason)] {
			t.Errorf("no case covers the reason %s", reason)
		}
	}
}

func TestSignatureVectors(t *testing.T) {
	vectors := loadVectors(t)
	verified := 0
	for _, c := range vectors.Cases {
		ok := t.Run(c.Name, func(t *testing.T) {
			rawBody := c.rawBody(t)
			if c.Body != nil && !bytes.Equal([]byte(*c.Body), rawBody) {
				t.Fatalf("body and body_base64 disagree")
			}
			result := VerifyWebhookSignature(VerifyWebhookSignatureInput{
				RawBody:          rawBody,
				SignatureHeader:  c.Headers["x-grualink-signature"],
				Secrets:          []string{c.Secret},
				NowSeconds:       c.Now,
				ToleranceSeconds: vectors.ToleranceSeconds,
			})
			if result.Valid != c.Expected.Valid || string(result.Reason) != c.Expected.Reason {
				t.Errorf("got (%v, %s), want (%v, %s): %s", result.Valid, result.Reason, c.Expected.Valid, c.Expected.Reason, c.Description)
			}
			if result.Valid != (result.Reason == ReasonOK) {
				t.Errorf("valid=%v with reason %s", result.Valid, result.Reason)
			}
			verified++
		})
		if !ok {
			t.Errorf("case %s failed", c.Name)
		}
	}
	// A case that is skipped or filtered out is a failure.
	if verified != len(vectors.Cases) {
		t.Fatalf("verified %d of %d cases: every case must run", verified, len(vectors.Cases))
	}
}

func TestTheReceiverMayHoldTwoSecretsDuringItsOwnRotation(t *testing.T) {
	vectors := loadVectors(t)
	c := findVector(t, vectors, "valid_basic")
	input := VerifyWebhookSignatureInput{
		RawBody:         c.rawBody(t),
		SignatureHeader: c.Headers["x-grualink-signature"],
		NowSeconds:      c.Now,
	}

	input.Secrets = []string{"whsec_TEST_ONLY_FAKE_NEXT", c.Secret}
	if result := VerifyWebhookSignature(input); !result.Valid || result.Reason != ReasonOK {
		t.Errorf("two secrets, one matching: %+v", result)
	}
	input.Secrets = []string{"", c.Secret}
	if result := VerifyWebhookSignature(input); !result.Valid {
		t.Errorf("an empty secret must be ignored: %+v", result)
	}
	for _, none := range [][]string{nil, {}, {""}, {"", ""}} {
		input.Secrets = none
		if result := VerifyWebhookSignature(input); result.Valid || result.Reason != ReasonMissingSecret {
			t.Errorf("secrets %q: %+v", none, result)
		}
	}
}

func TestTheSystemClockAndTheDefaultToleranceAreUsedWhenNotGiven(t *testing.T) {
	body := []byte(`{"id": "x"}`)
	secrets := []string{"whsec_TEST_ONLY_FAKE_A"}
	now := time.Now().Unix()
	for offset, want := range map[int64]SignatureVerificationReason{
		0:    ReasonOK,
		-200: ReasonOK,
		200:  ReasonOK,
		-400: ReasonTimestampOutOfTolerance,
		400:  ReasonTimestampOutOfTolerance,
	} {
		result := VerifyWebhookSignature(VerifyWebhookSignatureInput{
			RawBody: body, SignatureHeader: BuildSignatureHeader(secrets, now+offset, body), Secrets: secrets,
		})
		if result.Reason != want {
			t.Errorf("signed %d s from now: got %s, want %s", offset, result.Reason, want)
		}
	}
}

func TestVerificationNeverPanicsOnHostileInput(t *testing.T) {
	hostileHeaders := []string{
		"", " ", ",", "=", "t", "t=", "v1=", "t=,v1=", "=,=,=", "t==1", "\x00", "t=1,v1=\xff\xfe",
		strings.Repeat("x", 100_000),
		"t=1,v1=" + strings.Repeat("a", 64) + strings.Repeat(",", 5000),
		"t=999999999999,v1=" + strings.Repeat("a", 64),
		"t=9999999999999,v1=" + strings.Repeat("a", 64),
		"t=-1,v1=" + strings.Repeat("a", 64),
		"t=+1,v1=" + strings.Repeat("a", 64),
		"t= 1,v1=" + strings.Repeat("a", 64),
		"t=1,v1=" + strings.Repeat("a", 63) + "g",
		"t=1,v1=" + strings.Repeat("a", 65),
	}
	for _, header := range hostileHeaders {
		for _, secrets := range [][]string{nil, {""}, {"whsec_TEST_ONLY_FAKE_SECRET_0001"}} {
			for _, body := range [][]byte{nil, {}, []byte("{}"), {0xff, 0x00}} {
				result := VerifyWebhookSignature(VerifyWebhookSignatureInput{
					RawBody: body, SignatureHeader: header, Secrets: secrets, NowSeconds: 1,
				})
				if result.Valid || result.Reason == ReasonOK {
					t.Errorf("header %.40q was accepted", header)
				}
			}
		}
	}
}

func TestBuildSignatureHeaderProducesWhatTheVerifierAccepts(t *testing.T) {
	body := []byte(`{"id": "x"}`)
	secrets := []string{"whsec_TEST_ONLY_FAKE_A", "whsec_TEST_ONLY_FAKE_B"}
	header := BuildSignatureHeader(secrets, 1795000000, body)
	if !regexp.MustCompile(`^t=1795000000,v1=[0-9a-f]{64},v1=[0-9a-f]{64}$`).MatchString(header) {
		t.Fatalf("unexpected header: %s", header)
	}
	for _, secret := range secrets {
		result := VerifyWebhookSignature(VerifyWebhookSignatureInput{
			RawBody: body, SignatureHeader: header, Secrets: []string{secret}, NowSeconds: 1795000001,
		})
		if !result.Valid {
			t.Errorf("secret %s: %+v", secret, result)
		}
	}

	// The signing function agrees with the signature of a contract vector made elsewhere.
	vectors := loadVectors(t)
	c := findVector(t, vectors, "valid_basic")
	signature := ComputeWebhookSignature(c.Secret, 1795000000, c.rawBody(t))
	if !strings.Contains(c.Headers["x-grualink-signature"], "t=1795000000,v1="+signature) {
		t.Errorf("ComputeWebhookSignature disagrees with the contract vector")
	}
}
