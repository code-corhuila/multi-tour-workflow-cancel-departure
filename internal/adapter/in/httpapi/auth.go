package httpapi

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"slices"
	"strings"

	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const (
	// tokenLeeway is the clock tolerance of ADR-004 D10.
	tokenLeeway         = 30 * time.Second
	minKeyBits          = 2048
	tenantAdministrator = "TENANT_ADMINISTRATOR"
	serviceRole         = "SERVICE"
)

var (
	errMissingSubject = errors.New("token has no subject")
	errMalformedClaim = errors.New("token has a claim of the wrong type")
)

// Claims is what the routes may know about the caller.
type Claims struct {
	Subject string
	// Access marks a token that serves one purpose only ("restricted", "password-change"); empty for an ordinary one.
	Access   string
	Roles    []string
	TenantID string
}

// isTenantAdministrator is true for an ordinary token with the role and a tenant that is an id: the tenant goes into a path and a query.
func (c Claims) isTenantAdministrator() bool {
	return c.Access == "" && slices.Contains(c.Roles, tenantAdministrator) && saga.ValidID(c.TenantID)
}

// Verifier validates the access tokens of the security service with its public key only: this service never holds the private key
// (ADR-004 D5).
type Verifier struct {
	key              *rsa.PublicKey
	issuer, audience string
}

// NewVerifier expects a non-nil key; the composition root validates it at start-up.
func NewVerifier(key *rsa.PublicKey, issuer, audience string) *Verifier {
	return &Verifier{key: key, issuer: issuer, audience: audience}
}

// Verify accepts exactly RS256 and requires exp, sub, the issuer and the audience of the platform; every other algorithm, `none`
// included, is refused before the signature is looked at. A claim of the wrong type is refused, not read as absent.
func (v *Verifier) Verify(raw string) (Claims, error) {
	token, err := jwt.Parse(raw,
		func(*jwt.Token) (any, error) { return v.key, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithLeeway(tokenLeeway),
	)
	if err != nil {
		return Claims{}, err
	}
	subject, err := token.Claims.GetSubject()
	if err != nil || subject == "" {
		return Claims{}, errMissingSubject
	}
	claims := Claims{Subject: subject}
	body, _ := token.Claims.(jwt.MapClaims)
	for name, target := range map[string]*string{"access": &claims.Access, "tenantId": &claims.TenantID} {
		if value, present := body[name]; present && value != nil {
			text, isText := value.(string)
			if !isText {
				return Claims{}, errMalformedClaim
			}
			*target = text
		}
	}
	if listed, present := body["roles"]; present && listed != nil {
		list, isList := listed.([]any)
		if !isList {
			return Claims{}, errMalformedClaim
		}
		for _, role := range list {
			name, isText := role.(string)
			if !isText {
				return Claims{}, errMalformedClaim
			}
			claims.Roles = append(claims.Roles, name)
		}
	}
	return claims, nil
}

// isService is true for an ordinary token that is the role SERVICE alone and names no tenant, which is what a service of the platform carries; the
// tenant of its route is the one of the path.
func (c Claims) isService() bool {
	return c.Access == "" && c.TenantID == "" && slices.Equal(c.Roles, []string{serviceRole})
}

// ParsePublicKey reads the RSA public key of the security service from PEM (PKIX or PKCS#1).
func ParsePublicKey(data []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("the public key is not PEM")
	}
	var key any
	var err error
	if strings.Contains(block.Type, "RSA") {
		key, err = x509.ParsePKCS1PublicKey(block.Bytes)
	} else {
		key, err = x509.ParsePKIXPublicKey(block.Bytes)
	}
	if err != nil {
		return nil, errors.New("the public key cannot be read")
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok || rsaKey.N.BitLen() < minKeyBits {
		return nil, errors.New("the public key must be RSA of at least 2048 bits")
	}
	return rsaKey, nil
}
