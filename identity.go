package fabricsdk

import (
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hyperledger/fabric-gateway/pkg/identity"
)

// Identity is a Fabric client identity: an MSP ID, an X.509 certificate and a
// signing function. Load it once and reuse it for many calls; an Identity is
// safe for concurrent use.
//
// The private key never has to be in memory: NewIdentity accepts any signing
// function, and IdentityFromHSM (build tag "pkcs11") signs inside an HSM.
type Identity struct {
	x509  *identity.X509Identity
	cert  *x509.Certificate
	sign  identity.Sign
	close func() error
}

// NewIdentity builds an Identity from a certificate and any signing function,
// for example one backed by a remote signing service.
func NewIdentity(mspID string, cert *x509.Certificate, sign identity.Sign) (*Identity, error) {
	if strings.TrimSpace(mspID) == "" {
		return nil, configError(errors.New("identity: MSP ID is empty"))
	}
	if cert == nil {
		return nil, configError(errors.New("identity: certificate is nil"))
	}
	if sign == nil {
		return nil, configError(errors.New("identity: sign function is nil"))
	}
	id, err := identity.NewX509Identity(mspID, cert)
	if err != nil {
		return nil, configError(fmt.Errorf("identity: %w", err))
	}
	return &Identity{x509: id, cert: cert, sign: sign}, nil
}

// IdentityFromPEM builds an Identity from PEM-encoded certificate and private
// key bytes. It checks that the key belongs to the certificate.
func IdentityFromPEM(mspID string, certPEM, keyPEM []byte) (*Identity, error) {
	cert, err := identity.CertificateFromPEM(certPEM)
	if err != nil {
		return nil, configError(fmt.Errorf("identity: parse certificate: %w", err))
	}
	key, err := identity.PrivateKeyFromPEM(keyPEM)
	if err != nil {
		return nil, configError(fmt.Errorf("identity: parse private key: %w", err))
	}
	if err := checkKeyMatchesCert(key, cert); err != nil {
		return nil, configError(err)
	}
	sign, err := identity.NewPrivateKeySign(key)
	if err != nil {
		return nil, configError(fmt.Errorf("identity: create signer: %w", err))
	}
	return NewIdentity(mspID, cert, sign)
}

// LoadIdentity reads a certificate and private key from disk. Each path may be
// a PEM file, or an MSP directory (signcerts / keystore) that contains exactly
// one regular, non-hidden file. A directory with several files is an error,
// because picking one silently can load the wrong key.
func LoadIdentity(mspID, certPath, keyPath string) (*Identity, error) {
	certPEM, err := readPEMPath(certPath)
	if err != nil {
		return nil, configError(fmt.Errorf("identity: certificate: %w", err))
	}
	keyPEM, err := readPEMPath(keyPath)
	if err != nil {
		return nil, configError(fmt.Errorf("identity: private key: %w", err))
	}
	return IdentityFromPEM(mspID, certPEM, keyPEM)
}

// MSPID returns the identity's MSP ID.
func (i *Identity) MSPID() string { return i.x509.MspID() }

// Certificate returns the identity's X.509 certificate.
func (i *Identity) Certificate() *x509.Certificate { return i.cert }

// Close releases resources held by the signer (for example an HSM session).
// It is safe to call more than once and on identities that hold nothing.
func (i *Identity) Close() error {
	if i == nil || i.close == nil {
		return nil
	}
	c := i.close
	i.close = nil
	return c()
}

func checkKeyMatchesCert(key crypto.PrivateKey, cert *x509.Certificate) error {
	signer, ok := key.(crypto.Signer)
	if !ok {
		return errors.New("identity: private key type cannot sign")
	}
	pub, ok := cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(signer.Public()) {
		return errors.New("identity: private key does not match the certificate")
	}
	return nil
}

// readPEMPath reads a file, or the single regular file inside a directory.
func readPEMPath(p string) ([]byte, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return os.ReadFile(p)
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || !e.Type().IsRegular() {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)
	switch len(files) {
	case 0:
		return nil, fmt.Errorf("no files in directory %s", p)
	case 1:
		return os.ReadFile(filepath.Join(p, files[0]))
	default:
		return nil, fmt.Errorf("directory %s holds %d files (%s); pass the path of one file", p, len(files), strings.Join(files, ", "))
	}
}
