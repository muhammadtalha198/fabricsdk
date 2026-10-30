//go:build pkcs11

package fabricsdk

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/hyperledger/fabric-gateway/pkg/identity"
)

// HSMOptions locate a private key inside a PKCS#11 token.
type HSMOptions struct {
	// Label is the token label, e.g. "ForFabric".
	Label string
	// Pin is the user PIN of the token. Load it from a secret store, never
	// from source code.
	Pin string
	// Identifier is the key's CKA_ID. Leave empty to use the Subject Key
	// Identifier derived from the certificate, which is what fabric-ca-client
	// sets when it enrolls with a PKCS#11 BCCSP.
	Identifier []byte
}

// IdentityFromHSM builds an Identity whose private key stays inside an HSM.
// The factory should be created once per process with
// identity.NewHSMSignerFactory(libraryPath) and disposed at shutdown.
// Call Identity.Close when the identity is no longer needed.
//
// Signing is serialised per HSM signer by the underlying library, so a busy
// service should keep one Identity per officer rather than share one.
//
// Requires building with -tags pkcs11 and CGO_ENABLED=1.
func IdentityFromHSM(factory *identity.HSMSignerFactory, mspID string, certPEM []byte, opts HSMOptions) (*Identity, error) {
	if factory == nil {
		return nil, configError(errors.New("identity: HSM signer factory is nil"))
	}
	cert, err := identity.CertificateFromPEM(certPEM)
	if err != nil {
		return nil, configError(fmt.Errorf("identity: parse certificate: %w", err))
	}
	id := opts.Identifier
	if len(id) == 0 {
		pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			return nil, configError(errors.New("identity: HSM identities must use ECDSA certificates"))
		}
		ecdh, err := pub.ECDH()
		if err != nil {
			return nil, configError(fmt.Errorf("identity: convert public key: %w", err))
		}
		ski := sha256.Sum256(ecdh.Bytes())
		id = ski[:]
	}
	sign, closeFn, err := factory.NewHSMSigner(identity.HSMSignerOptions{
		Label:      opts.Label,
		Pin:        opts.Pin,
		Identifier: string(id),
	})
	if err != nil {
		return nil, configError(fmt.Errorf("identity: open HSM signer: %w", err))
	}
	ident, err := NewIdentity(mspID, cert, sign)
	if err != nil {
		_ = closeFn()
		return nil, err
	}
	ident.close = closeFn
	return ident, nil
}
