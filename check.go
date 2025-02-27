package jwt

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math/big"

	"github.com/buger/jsonparser"
)

// ErrSigMiss means the signature check failed.
var ErrSigMiss = errors.New("jwt: signature mismatch")

var errNoPayload = errors.New("jwt: one part only—payload absent")

// “Producers MUST NOT use the empty list "[]" as the "crit" value.”
// — “JSON Web Signature (JWS)” RFC 7515, subsection 4.1.11
var errCritEmpty = errors.New("jwt: empty array in crit header")

// EvalCrit is invoked by the Check functions for each token with one or more
// JOSE extensions. The crit slice has the JSON field names (for header) which
// “MUST be understood and processed” according to RFC 7515, subsection 4.1.11.
// “If any of the listed extension Header Parameters are not understood and
// supported by the recipient, then the JWS is invalid.”
// The respective Check function returns any error from EvalCrit as is.
var EvalCrit = func(token []byte, crit []string, header json.RawMessage) error {
	return fmt.Errorf("jwt: unsupported critical extension in JOSE header: %q", crit)
}

// ParseWithoutCheck skips the signature validation.
func ParseWithoutCheck(token []byte) (*Claims, error) {
	var c Claims
	_, _, _, err := c.scan(token)
	if err != nil {
		return nil, err
	}

	return &c, c.applyPayload()
}

// ECDSACheck parses a JWT if, and only if, the signature checks out.
// The return is an AlgError when the algorithm is not in ECDSAAlgs.
// Use Valid to complete the verification.
func ECDSACheck(token []byte, key *ecdsa.PublicKey) (*Claims, error) {
	var c Claims
	bodyLen, sig, alg, err := c.scan(token)
	if err != nil {
		return nil, err
	}

	hash, err := hashLookup(alg, ECDSAAlgs)
	if err != nil {
		return nil, err
	}
	digest := hash.New()
	digest.Write(token[:bodyLen])

	r := new(big.Int).SetBytes(sig[:len(sig)/2])
	s := new(big.Int).SetBytes(sig[len(sig)/2:])
	buf := sig[len(sig):]
	if !ecdsa.Verify(key, digest.Sum(buf), r, s) {
		return nil, ErrSigMiss
	}

	return &c, c.applyPayload()
}

// EdDSACheck parses a JWT if, and only if, the signature checks out.
// Use Valid to complete the verification.
func EdDSACheck(token []byte, key ed25519.PublicKey) (*Claims, error) {
	var c Claims
	bodyLen, sig, alg, err := c.scan(token)
	if err != nil {
		return nil, err
	}

	if alg != EdDSA {
		return nil, AlgError(alg)
	}

	if !ed25519.Verify(key, token[:bodyLen], sig) {
		return nil, ErrSigMiss
	}

	return &c, c.applyPayload()
}

// HMACCheck parses a JWT if, and only if, the signature checks out.
// The return is an AlgError when the algorithm is not in HMACAlgs.
// Use Valid to complete the verification.
func HMACCheck(token, secret []byte) (*Claims, error) {
	if len(secret) == 0 {
		return nil, errNoSecret
	}

	var c Claims
	bodyLen, sig, alg, err := c.scan(token)
	if err != nil {
		return nil, err
	}

	hash, err := hashLookup(alg, HMACAlgs)
	if err != nil {
		return nil, err
	}
	digest := hmac.New(hash.New, secret)
	digest.Write(token[:bodyLen])

	buf := sig[len(sig):]
	if !hmac.Equal(sig, digest.Sum(buf)) {
		return nil, ErrSigMiss
	}

	return &c, c.applyPayload()
}

// Check parses a JWT if, and only if, the signature checks out.
// The return is an AlgError when the algorithm does not match.
// Use Valid to complete the verification.
func (h *HMAC) Check(token []byte) (*Claims, error) {
	var c Claims
	bodyLen, sig, alg, err := c.scan(token)
	if err != nil {
		return nil, err
	}
	if alg != h.alg {
		return nil, AlgError(alg)
	}

	digest := h.digests.Get().(hash.Hash)
	defer h.digests.Put(digest)
	digest.Reset()
	digest.Write(token[:bodyLen])

	buf := sig[len(sig):]
	if !hmac.Equal(sig, digest.Sum(buf)) {
		return nil, ErrSigMiss
	}

	return &c, c.applyPayload()
}

// RSACheck parses a JWT if, and only if, the signature checks out.
// The return is an AlgError when the algorithm is not in RSAAlgs.
// Use Valid to complete the verification.
func RSACheck(token []byte, key *rsa.PublicKey) (*Claims, error) {
	var c Claims
	bodyLen, sig, alg, err := c.scan(token)
	if err != nil {
		return nil, err
	}

	hash, err := hashLookup(alg, RSAAlgs)
	if err != nil {
		return nil, err
	}
	digest := hash.New()
	digest.Write(token[:bodyLen])

	buf := sig[len(sig):]
	if alg != "" && alg[0] == 'P' {
		err = rsa.VerifyPSS(key, hash, digest.Sum(buf), sig, &pSSOptions)
	} else {
		err = rsa.VerifyPKCS1v15(key, hash, digest.Sum(buf), sig)
	}
	if err != nil {
		return nil, ErrSigMiss
	}

	return &c, c.applyPayload()
}

// DecodeParts reads up to three base64 parts. The result goes in c.RawHeader, c.Raw and sig.
func (c *Claims) decodeParts(token []byte) (bodyLen int, sig []byte, err error) {
	// fits all 3 parts decoded + buffer space for Hash.Sum.
	buf := make([]byte, len(token))

	// header
	i := bytes.IndexByte(token, '.')
	if i < 0 {
		i = len(token)
	}
	n, err := encoding.Decode(buf, token[:i])
	if err != nil {
		return 0, nil, fmt.Errorf("jwt: malformed JOSE header: %w", err)
	}
	c.RawHeader = json.RawMessage(buf[:n])
	buf = buf[n:]

	if i >= len(token) {
		return len(token), nil, nil
	}
	i++ // pass first dot

	// payload
	bodyLen = i + bytes.IndexByte(token[i:], '.')
	if bodyLen < i {
		bodyLen = len(token)
	}
	n, err = encoding.Decode(buf, token[i:bodyLen])
	if err != nil {
		return 0, nil, fmt.Errorf("jwt: malformed payload: %w", err)
	}
	c.Raw = json.RawMessage(buf[:n])
	buf = buf[n:]

	if bodyLen >= len(token) {
		return bodyLen, nil, nil
	}

	// signature
	remain := token[bodyLen+1:]
	end := bytes.IndexByte(remain, '.')
	if end >= 0 {
		remain = remain[:end]
	}
	n, err = encoding.Decode(buf, remain)
	if err != nil {
		return 0, nil, fmt.Errorf("jwt: malformed signature: %w", err)
	}
	return bodyLen, buf[:n], nil
}

func (c *Claims) scan(token []byte) (bodyLen int, sig []byte, alg string, err error) {
	bodyLen, sig, err = c.decodeParts(token)
	if err != nil {
		return 0, nil, "", err
	}

	var header struct {
		Kid  string   `json:"kid"`
		Alg  string   `json:"alg"`
		Crit []string `json:"crit"`
	}

	kid, valueType, _, err := jsonparser.Get([]byte(c.RawHeader), "kid")
	if err != nil && valueType != jsonparser.NotExist {
		return 0, nil, "", fmt.Errorf("jwt: malformed JOSE header: %w", err)
	}
	header.Kid = string(kid)

	algJwt, valueType, _, err := jsonparser.Get([]byte(c.RawHeader), "alg")
	if err != nil && valueType != jsonparser.NotExist {
		return 0, nil, "", fmt.Errorf("jwt: malformed JOSE header: %w", err)
	}
	header.Alg = string(algJwt)

	jsonparser.ArrayEach([]byte(c.RawHeader), func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
		if dataType == jsonparser.String {
			header.Crit = append(header.Crit, string(value))
		}
	}, "crit")

	if len(c.Raw) == 0 {
		return 0, nil, "", errNoPayload
	}

	// apply JOSE
	alg = header.Alg
	c.KeyID = header.Kid
	if header.Crit != nil {
		if len(header.Crit) == 0 {
			return 0, nil, "", errCritEmpty
		}
		if err := EvalCrit(token, header.Crit, c.RawHeader); err != nil {
			return 0, nil, "", err
		}
	}

	return
}

func (c *Claims) applyPayload() error {
	err := jsonparser.ObjectEach([]byte(c.Raw), func(key, value []byte, dataType jsonparser.ValueType, offset int) error {
		skey := string(key)
		svalue := string(value)
		var err error

		if skey == issuer {
			c.Issuer = svalue
		} else if skey == subject {
			c.Subject = svalue
		} else if skey == audience && dataType == jsonparser.String {
			c.Audiences = []string{svalue}
		} else if skey == expires {
			var expiresFloat float64
			expiresFloat, err = jsonparser.GetFloat(value)

			c.Expires = (*NumericTime)(&expiresFloat)
		} else if skey == issued {
			var issuedAt float64
			issuedAt, err = jsonparser.GetFloat(value)

			c.Issued = (*NumericTime)(&issuedAt)
		} else if skey == id {
			c.ID = svalue
		}

		return err
	})

	return err
}
