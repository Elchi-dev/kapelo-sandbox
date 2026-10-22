package protocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

// Each case is one way an attacker with access to the connection, or a
// confused control plane, could try to make a sandbox run something.
func TestVerifyRefuses(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	v := &Verifier{Key: pub, SandboxID: "sb_1"}

	good, _ := Sign(priv, "sb_1", Status, nil, time.Minute)
	if _, err := v.Verify(good); err != nil {
		t.Fatalf("good command refused: %v", err)
	}
	if _, err := v.Verify(good); !errors.Is(err, ErrReplay) {
		t.Errorf("replayed command: %v, want ErrReplay", err)
	}

	forged, _ := Sign(other, "sb_1", Status, nil, time.Minute)
	if _, err := v.Verify(forged); !errors.Is(err, ErrSignature) {
		t.Errorf("command signed by another key: %v", err)
	}

	tampered, _ := Sign(priv, "sb_1", Status, nil, time.Minute)
	tampered.Envelope = append([]byte(nil), tampered.Envelope...)
	tampered.Envelope[len(tampered.Envelope)-2] ^= 1
	if _, err := v.Verify(tampered); !errors.Is(err, ErrSignature) {
		t.Errorf("tampered command: %v", err)
	}

	elsewhere, _ := Sign(priv, "sb_2", Status, nil, time.Minute)
	if _, err := v.Verify(elsewhere); !errors.Is(err, ErrSandbox) {
		t.Errorf("command for another sandbox: %v", err)
	}

	old, _ := Sign(priv, "sb_1", Status, nil, time.Minute)
	late := &Verifier{Key: pub, SandboxID: "sb_1", Now: func() time.Time { return time.Now().Add(10 * time.Minute) }}
	if _, err := late.Verify(old); !errors.Is(err, ErrExpired) {
		t.Errorf("expired command: %v", err)
	}
}
