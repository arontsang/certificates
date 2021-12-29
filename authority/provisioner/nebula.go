package provisioner

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"net"
	"time"

	"github.com/pkg/errors"
	"github.com/slackhq/nebula/cert"
	"github.com/smallstep/certificates/errs"
	"go.step.sm/crypto/jose"
	"go.step.sm/crypto/sshutil"
	"go.step.sm/crypto/x25519"
	"go.step.sm/crypto/x509util"
	"golang.org/x/crypto/ssh"
)

const (
	// NebulaCertHeader is the token header that contains a nebula certificate.
	NebulaCertHeader jose.HeaderKey = "nbc"
)

type Nebula struct {
	ID        string   `json:"-"`
	Type      string   `json:"type"`
	Name      string   `json:"name"`
	Roots     []byte   `json:"roots"`
	Claims    *Claims  `json:"claims,omitempty"`
	Options   *Options `json:"options,omitempty"`
	claimer   *Claimer
	caPool    *cert.NebulaCAPool
	audiences Audiences
}

func (p *Nebula) Init(config Config) error {
	switch {
	case p.Type == "":
		return errors.New("provisioner type cannot be empty")
	case p.Name == "":
		return errors.New("provisioner name cannot be empty")
	case len(p.Roots) == 0:
		return errors.New("provisioner root(s) cannot be empty")
	}

	var err error
	if p.claimer, err = NewClaimer(p.Claims, config.Claims); err != nil {
		return err
	}

	p.caPool, err = cert.NewCAPoolFromBytes(p.Roots)
	if err != nil {
		return errs.InternalServer("failed to start ca pool: %v", err)
	}

	p.audiences = config.Audiences.WithFragment(p.GetIDForToken())

	return nil
}

// GetID returns the provisioner id.
func (p *Nebula) GetID() string {
	if p.ID != "" {
		return p.ID
	}
	return p.GetIDForToken()
}

// GetIDForToken returns an identifier that will be used to load the provisioner
// from a token.
func (p *Nebula) GetIDForToken() string {
	return "nebula/" + p.Name
}

// GetTokenID returns the identifier of the token.
func (p *Nebula) GetTokenID(token string) (string, error) {
	// Validate payload
	t, err := jose.ParseSigned(token)
	if err != nil {
		return "", errors.Wrap(err, "error parsing token")
	}

	// Get claims w/out verification. We need to look up the provisioner
	// key in order to verify the claims and we need the issuer from the claims
	// before we can look up the provisioner.
	var claims jose.Claims
	if err = t.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return "", errors.Wrap(err, "error verifying claims")
	}
	return claims.ID, nil
}

// GetName returns the name of the provisioner.
func (p *Nebula) GetName() string {
	return p.Name
}

// GetType returns the type of provisioner.
func (p *Nebula) GetType() Type {
	return TypeNebula
}

// GetEncryptedKey returns the base provisioner encrypted key if it's defined.
func (p *Nebula) GetEncryptedKey() (kid string, key string, ok bool) {
	return "", "", false
}

// AuthorizeSign returns the list of SignOption for a Sign request.
func (p *Nebula) AuthorizeSign(ctx context.Context, token string) ([]SignOption, error) {
	cert, claims, err := p.authorizeToken(token, p.audiences.Sign)
	if err != nil {
		return nil, err
	}

	data := x509util.CreateTemplateData(claims.Subject, claims.SANs)
	if v, err := unsafeParseSigned(token); err == nil {
		data.SetToken(v)
	}
	data.Set("Cert", cert)

	templateOptions, err := TemplateOptions(p.Options, data)
	if err != nil {
		return nil, err
	}

	return []SignOption{
		templateOptions,
		// modifiers / withOptions
		newProvisionerExtensionOption(TypeNebula, p.Name, ""),
		profileLimitDuration{
			def:       p.claimer.DefaultTLSCertDuration(),
			notBefore: cert.Details.NotBefore,
			notAfter:  cert.Details.NotAfter,
		},
		// validators
		commonNameValidator(claims.Subject),
		nebulaSANsValidator{
			Name: cert.Details.Name,
			IPs:  cert.Details.Ips,
		},
		defaultPublicKeyValidator{},
		newValidityValidator(p.claimer.MinTLSCertDuration(), p.claimer.MaxTLSCertDuration()),
	}, nil
}

// AuthorizeSSHSign returns the list of SignOption for a SignSSH request.
// Currently the nebula provisioner only grant host ssh certificates
func (p *Nebula) AuthorizeSSHSign(ctx context.Context, token string) ([]SignOption, error) {
	if !p.claimer.IsSSHCAEnabled() {
		return nil, errs.Unauthorized("ssh is disabled for nebula provisioner '%s'", p.Name)
	}

	cert, claims, err := p.authorizeToken(token, p.audiences.SSHSign)
	if err != nil {
		return nil, err
	}

	// Default template attributes.
	keyID := claims.Subject
	principals := make([]string, len(cert.Details.Ips)+1)
	principals[0] = cert.Details.Name
	for i, ipnet := range cert.Details.Ips {
		principals[i+1] = ipnet.IP.String()
	}

	var signOptions []SignOption
	// If step ssh options are given, validate them and set key id, principals
	// and validity.
	if claims.Step != nil || claims.Step.SSH != nil {
		opts := claims.Step.SSH

		// Check that the token only contains valid principals.
		v := nebulaPrincipalsValidator{
			Name: cert.Details.Name,
			IPs:  cert.Details.Ips,
		}
		if err := v.Valid(*opts); err != nil {
			return nil, err
		}
		// Check that the cert type is a valid one.
		if opts.CertType != "" && opts.CertType != SSHHostCert {
			return nil, errs.Forbidden("ssh certificate type does not match - got %v, want %v", opts.CertType, SSHHostCert)
		}

		signOptions = []SignOption{
			// validate is a host certificate and users's KeyID is the subject.
			sshCertOptionsValidator(SignSSHOptions{
				CertType: SSHHostCert,
				KeyID:    claims.Subject,
			}),
			// validates user's SSHOptions with the ones in the token
			sshCertOptionsValidator(*opts),
		}

		// Use options in the token.
		if opts.KeyID != "" {
			keyID = opts.KeyID
		}
		if len(opts.Principals) > 0 {
			principals = opts.Principals
		}

		// Add modifiers from custom claims
		t := now()
		if !opts.ValidAfter.IsZero() {
			signOptions = append(signOptions, sshCertValidAfterModifier(opts.ValidAfter.RelativeTime(t).Unix()))
		}
		if !opts.ValidBefore.IsZero() {
			signOptions = append(signOptions, sshCertValidBeforeModifier(opts.ValidBefore.RelativeTime(t).Unix()))
		}
	}

	// Certificate templates.
	data := sshutil.CreateTemplateData(sshutil.HostCert, keyID, principals)
	if v, err := unsafeParseSigned(token); err == nil {
		data.SetToken(v)
	}
	data.Set("Cert", cert)

	templateOptions, err := TemplateSSHOptions(p.Options, data)
	if err != nil {
		return nil, err
	}

	return append(signOptions,
		templateOptions,
		// Checks the validity bounds, and set the validity if has not been set.
		&sshLimitDuration{p.claimer, cert.Details.NotAfter},
		// Validate public key.
		&sshDefaultPublicKeyValidator{},
		// Validate the validity period.
		&sshCertValidityValidator{p.claimer},
		// Require all the fields in the SSH certificate
		&sshCertDefaultValidator{},
	), nil
}

// AuthorizeRenew returns an error if the renewal is disabled.
func (p *Nebula) AuthorizeRenew(ctx context.Context, cert *x509.Certificate) error {
	if p.claimer.IsDisableRenewal() {
		return errs.Unauthorized("renew is disabled for nebula provisioner '%s'", p.GetName())
	}
	return nil
}

// AuthorizeRevoke returns an error if the token is not valid.
func (p *Nebula) AuthorizeRevoke(ctx context.Context, token string) error {
	return p.validateToken(token, p.audiences.Revoke)
}

// AuthorizeSSHRevoke returns an error if SSH is disabled or the token is invalid.
func (p *Nebula) AuthorizeSSHRevoke(ctx context.Context, token string) error {
	if !p.claimer.IsSSHCAEnabled() {
		return errs.Unauthorized("ssh is disabled for nebula provisioner '%s'", p.Name)
	}
	if _, _, err := p.authorizeToken(token, p.audiences.Revoke); err != nil {
		return err
	}
	return nil
}

// AuthorizeSSHRenew returns an unauthorized error.
func (p *Nebula) AuthorizeSSHRenew(ctx context.Context, token string) (*ssh.Certificate, error) {
	return nil, errs.Unauthorized("nebula provisioner does not support SSH renew")
}

// AuthorizeSSHRekey returns an unauthorized error.
func (p *Nebula) AuthorizeSSHRekey(ctx context.Context, token string) (*ssh.Certificate, []SignOption, error) {
	return nil, nil, errs.Unauthorized("nebula provisioner does not support SSH rekey")
}

func (p *Nebula) validateToken(token string, audiences []string) error {
	_, _, err := p.authorizeToken(token, audiences)
	return err
}

func (p *Nebula) authorizeToken(token string, audiences []string) (*cert.NebulaCertificate, *jwtPayload, error) {
	jwt, err := jose.ParseSigned(token)
	if err != nil {
		return nil, nil, errs.UnauthorizedErr(err, errs.WithMessage("failed to parse token"))
	}

	// Extract nebula certificate
	nbc, ok := jwt.Headers[0].ExtraHeaders[NebulaCertHeader]
	if !ok {
		return nil, nil, errs.Unauthorized("failed to parse token: nbc header is missing")
	}
	s, ok := nbc.(string)
	if !ok {
		return nil, nil, errs.Unauthorized("failed to parse token: nbc header is not valid")
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, nil, errs.UnauthorizedErr(err, errs.WithMessage("failed to parse token: nbc header is not valid"))
	}
	c, _, err := cert.UnmarshalNebulaCertificateFromPEM(b)
	if err != nil {
		return nil, nil, errs.UnauthorizedErr(err, errs.WithMessage("failed to parse nebula certificate: nbc header is not valid"))
	}

	// Validate nebula certificate against CA
	if valid, err := c.Verify(now(), p.caPool); !valid {
		if err != nil {
			return nil, nil, errs.UnauthorizedErr(err, errs.WithMessage("token is not valid: failed to unmarshal certificate"))
		}
		return nil, nil, errs.Unauthorized("token is not valid: failed to unmarshal certificate")
	}

	var pub interface{}
	if c.Details.IsCA {
		pub = ed25519.PublicKey(c.Details.PublicKey)
	} else {
		pub = x25519.PublicKey(c.Details.PublicKey)
	}

	// Validate token with public key
	var claims jwtPayload
	if err := jose.Verify(jwt, pub, &claims); err != nil {
		return nil, nil, errs.UnauthorizedErr(err, errs.WithMessage("token is not valid: signature does not match"))
	}

	// According to "rfc7519 JSON Web Token" acceptable skew should be no
	// more than a few minutes.
	if err = claims.ValidateWithLeeway(jose.Expected{
		Issuer: p.Name,
		Time:   now(),
	}, time.Minute); err != nil {
		return nil, nil, errs.UnauthorizedErr(err, errs.WithMessage("token is not valid: invalid claims"))
	}
	// Validate token and subject too.
	if !matchesAudience(claims.Audience, audiences) {
		return nil, nil, errs.Unauthorized("token is not valid: invalid claims")
	}
	if claims.Subject == "" {
		return nil, nil, errs.Unauthorized("token is not valid: subject cannot be empty")
	}

	return c, &claims, nil
}

type nebulaSANsValidator struct {
	Name string
	IPs  []*net.IPNet
}

// Valid verifies that the SANs stored in the validator are contained with those
// requested in the x509 certificate request.
func (v nebulaSANsValidator) Valid(req *x509.CertificateRequest) error {
	dnsNames, ips, emails, uris := x509util.SplitSANs([]string{v.Name})
	if len(req.DNSNames) > 0 {
		if err := dnsNamesValidator(dnsNames).Valid(req); err != nil {
			return err
		}
	}
	if len(req.EmailAddresses) > 0 {
		if err := emailAddressesValidator(emails).Valid(req); err != nil {
			return err
		}
	}
	if len(req.URIs) > 0 {
		if err := urisValidator(uris).Valid(req); err != nil {
			return err
		}
	}
	if len(req.IPAddresses) > 0 {
		for _, ip := range req.IPAddresses {
			var valid bool
			// Check ip in name
			for _, ipInName := range ips {
				if ip.Equal(ipInName) {
					valid = true
					break
				}
			}
			// Check ip network
			if !valid {
				for _, ipnet := range v.IPs {
					if ip.Equal(ipnet.IP) {
						valid = true
						break
					}
				}
			}
			if !valid {
				return errs.Forbidden("certificate request does not contain the valid IP addresses - got %v, want %v", req.IPAddresses, v.IPs)
			}
		}
	}

	return nil
}

type nebulaPrincipalsValidator struct {
	Name string
	IPs  []*net.IPNet
}

// Valid checks that the SignSSHOptions principals contains only names in the
// nebula certificate.
func (v nebulaPrincipalsValidator) Valid(got SignSSHOptions) error {
	for _, p := range got.Principals {
		var valid bool
		if p == v.Name {
			valid = true
		}
		if !valid {
			if ip := net.ParseIP(p); ip != nil {
				for _, ipnet := range v.IPs {
					if ip.Equal(ipnet.IP) {
						valid = true
						break
					}
				}
			}
		}

		if !valid {
			return errs.Forbidden(
				"ssh certificate principals does contain a valid name or IP address - got %v, want %s or %v",
				got.Principals, v.Name, v.IPs,
			)
		}
	}
	return nil
}
