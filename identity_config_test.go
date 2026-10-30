package fabricsdk_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muhammadtalha198/fabricsdk"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadIdentityFromMSPDirectories(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := newTestIdentityPEM(t, "user1")
	writeFile(t, filepath.Join(dir, "signcerts", "cert.pem"), certPEM)
	writeFile(t, filepath.Join(dir, "signcerts", ".DS_Store"), []byte("junk"))
	writeFile(t, filepath.Join(dir, "keystore", "abc_sk"), keyPEM)

	id, err := fabricsdk.LoadIdentity("Org1MSP", filepath.Join(dir, "signcerts"), filepath.Join(dir, "keystore"))
	if err != nil {
		t.Fatal(err)
	}
	if id.MSPID() != "Org1MSP" || id.Certificate().Subject.CommonName != "user1" {
		t.Fatalf("unexpected identity %s %s", id.MSPID(), id.Certificate().Subject.CommonName)
	}

	// File paths work too.
	if _, err := fabricsdk.LoadIdentity("Org1MSP", filepath.Join(dir, "signcerts", "cert.pem"), filepath.Join(dir, "keystore", "abc_sk")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadIdentityRejectsAmbiguousKeystore(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := newTestIdentityPEM(t, "user1")
	_, otherKey := newTestIdentityPEM(t, "user2")
	writeFile(t, filepath.Join(dir, "signcerts", "cert.pem"), certPEM)
	writeFile(t, filepath.Join(dir, "keystore", "a_sk"), keyPEM)
	writeFile(t, filepath.Join(dir, "keystore", "b_sk"), otherKey)

	_, err := fabricsdk.LoadIdentity("Org1MSP", filepath.Join(dir, "signcerts"), filepath.Join(dir, "keystore"))
	if fabricsdk.KindOf(err) != fabricsdk.KindConfig || !strings.Contains(err.Error(), "holds 2 files") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
}

func TestIdentityFromPEMRejectsMismatchedKey(t *testing.T) {
	certPEM, _ := newTestIdentityPEM(t, "user1")
	_, otherKey := newTestIdentityPEM(t, "user2")
	_, err := fabricsdk.IdentityFromPEM("Org1MSP", certPEM, otherKey)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("want mismatch error, got %v", err)
	}
}

func TestNewIdentityValidation(t *testing.T) {
	if _, err := fabricsdk.NewIdentity("", nil, nil); fabricsdk.KindOf(err) != fabricsdk.KindConfig {
		t.Fatalf("want config error, got %v", err)
	}
	var nilID *fabricsdk.Identity
	if err := nilID.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidation(t *testing.T) {
	_, err := fabricsdk.New(fabricsdk.Config{})
	if fabricsdk.KindOf(err) != fabricsdk.KindConfig {
		t.Fatalf("want config error, got %v", err)
	}
	for _, want := range []string{"PeerEndpoint", "TLSCertPath", "MSPID", "ChannelName", "ChaincodeName"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestNewBuildsTLSClientWithoutNetwork(t *testing.T) {
	dir := t.TempDir()
	caPEM, _ := newTestIdentityPEM(t, "tlsca")
	certPEM, keyPEM := newTestIdentityPEM(t, "user1")
	writeFile(t, filepath.Join(dir, "ca.pem"), caPEM)
	writeFile(t, filepath.Join(dir, "cert.pem"), certPEM)
	writeFile(t, filepath.Join(dir, "key.pem"), keyPEM)

	cfg := fabricsdk.Config{
		PeerEndpoint:      "peer0.org1.example.com:7051",
		TLSCertPath:       filepath.Join(dir, "ca.pem"),
		ClientTLSCertPath: filepath.Join(dir, "cert.pem"),
		ClientTLSKeyPath:  filepath.Join(dir, "key.pem"),
		MSPID:             "Org1MSP",
		CertPath:          filepath.Join(dir, "cert.pem"),
		KeyPath:           filepath.Join(dir, "key.pem"),
		ChannelName:       "mychannel",
		ChaincodeName:     "basic",
	}
	fc, err := fabricsdk.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := fc.Close(); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(dir, "bad.pem"), []byte("not a certificate"))
	cfg.TLSCertPath = filepath.Join(dir, "bad.pem")
	if _, err := fabricsdk.New(cfg); fabricsdk.KindOf(err) != fabricsdk.KindConfig {
		t.Fatalf("want config error for bad CA file, got %v", err)
	}

	cfg.TLSCertPath = filepath.Join(dir, "ca.pem")
	cfg.ClientTLSKeyPath = ""
	if _, err := fabricsdk.New(cfg); fabricsdk.KindOf(err) != fabricsdk.KindConfig {
		t.Fatalf("want config error for half mTLS config, got %v", err)
	}
}

func TestNilIdentityHandleFailsCleanly(t *testing.T) {
	fc := newTestClient(t, &fakeGateway{})
	_, err := fc.WithIdentity(nil).Evaluate(context.Background(), "Read")
	if fabricsdk.KindOf(err) != fabricsdk.KindConfig {
		t.Fatalf("want config error, got %v", err)
	}
}
