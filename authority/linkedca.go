package authority

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/smallstep/certificates/errs"
	"go.step.sm/crypto/jose"
	"go.step.sm/crypto/keyutil"
	"go.step.sm/crypto/tlsutil"
	"go.step.sm/crypto/x509util"
	"go.step.sm/linkedca"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const uuidPattern = "^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$"

type linkedCaClient struct {
	renewer     *tlsutil.Renewer
	client      linkedca.MajordomoClient
	authorityID string
}

type linkedCAClaims struct {
	jose.Claims
	SANs []string `json:"sans"`
	SHA  string   `json:"sha"`
}

func newLinkedCAClient(token string) (*linkedCaClient, error) {
	tok, err := jose.ParseSigned(token)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing token")
	}

	var claims linkedCAClaims
	if err := tok.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return nil, errors.Wrap(err, "error parsing token")
	}
	// Validate claims
	if len(claims.Audience) != 1 {
		return nil, errors.New("error parsing token: invalid aud claim")
	}
	if claims.SHA == "" {
		return nil, errors.New("error parsing token: invalid sha claim")
	}
	// Get linkedCA endpoint from audience.
	u, err := url.Parse(claims.Audience[0])
	if err != nil {
		return nil, errors.New("error parsing token: invalid aud claim")
	}
	// Get authority from SANs
	authority, err := getAuthority(claims.SANs)
	if err != nil {
		return nil, err
	}

	// Create csr to login with
	signer, err := keyutil.GenerateDefaultSigner()
	if err != nil {
		return nil, err
	}
	csr, err := x509util.CreateCertificateRequest(claims.Subject, claims.SANs, signer)
	if err != nil {
		return nil, err
	}

	// Get and verify root certificate
	root, err := getRootCertificate(u.Host, claims.SHA)
	if err != nil {
		return nil, err
	}

	pool := x509.NewCertPool()
	pool.AddCert(root)

	// Login with majordomo and get certificates
	cert, tlsConfig, err := login(authority, token, csr, signer, u.Host, pool)
	if err != nil {
		return nil, err
	}

	// Start TLS renewer and set the GetClientCertificate callback to it.
	renewer, err := tlsutil.NewRenewer(cert, tlsConfig, func() (*tls.Certificate, *tls.Config, error) {
		return login(authority, token, csr, signer, u.Host, pool)
	})
	if err != nil {
		return nil, err
	}
	tlsConfig.GetClientCertificate = renewer.GetClientCertificate

	// Start mTLS client
	conn, err := grpc.Dial(u.Host, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		return nil, errors.Wrapf(err, "error connecting %s", u.Host)
	}

	return &linkedCaClient{
		renewer:     renewer,
		client:      linkedca.NewMajordomoClient(conn),
		authorityID: authority,
	}, nil
}

func (c *linkedCaClient) Run() {
	c.renewer.Run()
}

func (c *linkedCaClient) Stop() {
	c.renewer.Stop()
}

func (c *linkedCaClient) CreateProvisioner(ctx context.Context, prov *linkedca.Provisioner) error {
	resp, err := c.client.CreateProvisioner(ctx, &linkedca.CreateProvisionerRequest{
		Type:         prov.Type,
		Name:         prov.Name,
		Details:      prov.Details,
		Claims:       prov.Claims,
		X509Template: prov.X509Template,
		SshTemplate:  prov.SshTemplate,
	})
	if err != nil {
		return errors.Wrap(err, "error creating provisioner")
	}
	prov.Id = resp.Id
	prov.AuthorityId = resp.AuthorityId
	return nil
}

func (c *linkedCaClient) GetProvisioner(ctx context.Context, id string) (*linkedca.Provisioner, error) {
	resp, err := c.client.GetConfiguration(ctx, &linkedca.ConfigurationRequest{
		AuthorityId: c.authorityID,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error getting provisioners")
	}
	for _, p := range resp.Provisioners {
		if p.Id == id {
			return p, nil
		}
	}
	return nil, errs.NotFound("provisioner not found")
}

func (c *linkedCaClient) GetProvisioners(ctx context.Context) ([]*linkedca.Provisioner, error) {
	resp, err := c.client.GetConfiguration(ctx, &linkedca.ConfigurationRequest{
		AuthorityId: c.authorityID,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error getting provisioners")
	}
	return resp.Provisioners, nil
}

func (c *linkedCaClient) UpdateProvisioner(ctx context.Context, prov *linkedca.Provisioner) error {
	_, err := c.client.UpdateProvisioner(ctx, &linkedca.UpdateProvisionerRequest{
		Id:           prov.Id,
		Name:         prov.Name,
		Details:      prov.Details,
		Claims:       prov.Claims,
		X509Template: prov.X509Template,
		SshTemplate:  prov.SshTemplate,
	})
	return errors.Wrap(err, "error updating provisioner")
}

func (c *linkedCaClient) DeleteProvisioner(ctx context.Context, id string) error {
	_, err := c.client.DeleteProvisioner(ctx, &linkedca.DeleteProvisionerRequest{
		Id: id,
	})
	return errors.Wrap(err, "error deleting provisioner")
}

func (c *linkedCaClient) CreateAdmin(ctx context.Context, adm *linkedca.Admin) error {
	resp, err := c.client.CreateAdmin(ctx, &linkedca.CreateAdminRequest{
		Subject:       adm.Subject,
		ProvisionerId: adm.ProvisionerId,
		Type:          adm.Type,
	})
	if err != nil {
		return errors.Wrap(err, "error creating admin")
	}
	adm.Id = resp.Id
	adm.AuthorityId = resp.AuthorityId
	return nil
}

func (c *linkedCaClient) GetAdmin(ctx context.Context, id string) (*linkedca.Admin, error) {
	resp, err := c.client.GetConfiguration(ctx, &linkedca.ConfigurationRequest{
		AuthorityId: c.authorityID,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error getting admins")
	}
	for _, a := range resp.Admins {
		if a.Id == id {
			return a, nil
		}
	}
	return nil, errs.NotFound("admin not found")
}

func (c *linkedCaClient) GetAdmins(ctx context.Context) ([]*linkedca.Admin, error) {
	resp, err := c.client.GetConfiguration(ctx, &linkedca.ConfigurationRequest{
		AuthorityId: c.authorityID,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error getting admins")
	}
	return resp.Admins, nil
}

func (c *linkedCaClient) UpdateAdmin(ctx context.Context, adm *linkedca.Admin) error {
	_, err := c.client.UpdateAdmin(ctx, &linkedca.UpdateAdminRequest{
		Id:   adm.Id,
		Type: adm.Type,
	})
	return errors.Wrap(err, "error updating admin")
}

func (c *linkedCaClient) DeleteAdmin(ctx context.Context, id string) error {
	_, err := c.client.DeleteAdmin(ctx, &linkedca.DeleteAdminRequest{
		Id: id,
	})
	return errors.Wrap(err, "error deleting admin")
}

func getAuthority(sans []string) (string, error) {
	for _, s := range sans {
		if strings.HasPrefix(s, "urn:smallstep:authority:") {
			if regexp.MustCompile(uuidPattern).MatchString(s[24:]) {
				return s[24:], nil
			}
		}
	}
	return "", fmt.Errorf("error parsing token: invalid sans claim")
}

// getRootCertificate creates an insecure majordomo client and returns the
// verified root certificate.
func getRootCertificate(endpoint, fingerprint string) (*x509.Certificate, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := grpc.DialContext(ctx, endpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		InsecureSkipVerify: true,
	})))
	if err != nil {
		return nil, errors.Wrapf(err, "error connecting %s", endpoint)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := linkedca.NewMajordomoClient(conn)
	resp, err := client.GetRootCertificate(ctx, &linkedca.GetRootCertificateRequest{
		Fingerprint: fingerprint,
	})
	if err != nil {
		return nil, fmt.Errorf("error getting root certificate: %w", err)
	}

	var block *pem.Block
	b := []byte(resp.PemCertificate)
	for len(b) > 0 {
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("error parsing certificate: %w", err)
		}

		// verify the sha256
		sum := sha256.Sum256(cert.Raw)
		if !strings.EqualFold(fingerprint, hex.EncodeToString(sum[:])) {
			return nil, fmt.Errorf("error verifying certificate: SHA256 fingerprint does not match")
		}

		return cert, nil
	}

	return nil, fmt.Errorf("error getting root certificate: certificate not found")
}

// login creates a new majordomo client with just the root ca pool and returns
// the signed certificate and tls configuration.
func login(authority, token string, csr *x509.CertificateRequest, signer crypto.PrivateKey, endpoint string, rootCAs *x509.CertPool) (*tls.Certificate, *tls.Config, error) {
	// Connect to majordomo
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := grpc.DialContext(ctx, endpoint, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		RootCAs: rootCAs,
	})))
	if err != nil {
		return nil, nil, errors.Wrapf(err, "error connecting %s", endpoint)
	}

	// Login to get the signed certificate
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := linkedca.NewMajordomoClient(conn)
	resp, err := client.Login(ctx, &linkedca.LoginRequest{
		AuthorityId: authority,
		Token:       token,
		PemCertificateRequest: string(pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE REQUEST",
			Bytes: csr.Raw,
		})),
	})
	if err != nil {
		return nil, nil, errors.Wrapf(err, "error logging in %s", endpoint)
	}

	// Parse login response
	var block *pem.Block
	var bundle []*x509.Certificate
	rest := []byte(resp.PemCertificateChain)
	for {
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, nil, errors.New("error decoding login response: pemCertificateChain is not a certificate bundle")
		}
		crt, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, errors.Wrap(err, "error parsing login response")
		}
		bundle = append(bundle, crt)
	}
	if len(bundle) == 0 {
		return nil, nil, errors.New("error decoding login response: pemCertificateChain should not be empty")
	}

	// Build tls.Certificate with PemCertificate and intermediates in the
	// PemCertificateChain
	cert := &tls.Certificate{
		PrivateKey: signer,
	}
	rest = []byte(resp.PemCertificate)
	for {
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, nil, errors.Wrap(err, "error parsing pemCertificate")
			}
			cert.Certificate = append(cert.Certificate, block.Bytes)
			cert.Leaf = leaf
		}
	}

	// Add intermediates to the tls.Certificate
	last := len(bundle) - 1
	for i := 0; i < last; i++ {
		cert.Certificate = append(cert.Certificate, bundle[i].Raw)
	}

	// Add root to the pool if it's not there yet
	rootCAs.AddCert(bundle[last])

	return cert, &tls.Config{
		RootCAs: rootCAs,
	}, nil
}
