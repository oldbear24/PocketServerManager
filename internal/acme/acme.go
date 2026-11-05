// Package acme gets certificates from an ACME certificate authority with lego
// (ADR 0001). The panel decides when to issue, for which names and with which
// challenge; this package registers one account or issues one certificate when
// it is asked to.
package acme

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	legoacme "github.com/go-acme/lego/v4/acme"
	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	legolog "github.com/go-acme/lego/v4/log"
	"github.com/go-acme/lego/v4/registration"
)

// Directories that PocketCloud knows by name.
const (
	LetsEncrypt        = lego.LEDirectoryProduction
	LetsEncryptStaging = lego.LEDirectoryStaging
	ZeroSSL            = "https://acme.zerossl.com/v2/DV90"
)

func init() {
	// The panel writes a DNS-01 answer in the zone of the name itself, so
	// lego must not follow a CNAME of _acme-challenge into another zone and
	// wait for the answer there.
	_ = os.Setenv("LEGO_DISABLE_CNAME_SUPPORT", "true")
	legolog.Logger = slogLogger{}
}

// slogLogger sends lego's chatter to the panel's log, at debug level.
type slogLogger struct{}

func (slogLogger) Fatal(args ...any)   { slog.Error(fmt.Sprint(args...), "component", "lego") }
func (slogLogger) Fatalln(args ...any) { slog.Error(fmt.Sprint(args...), "component", "lego") }
func (slogLogger) Fatalf(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...), "component", "lego")
}
func (slogLogger) Print(args ...any)   { slog.Debug(fmt.Sprint(args...), "component", "lego") }
func (slogLogger) Println(args ...any) { slog.Debug(fmt.Sprint(args...), "component", "lego") }
func (slogLogger) Printf(format string, args ...any) {
	slog.Debug(fmt.Sprintf(format, args...), "component", "lego")
}

// Account is an account at a certificate authority.
type Account struct {
	DirectoryURL string
	Email        string
	Key          crypto.Signer
	// URI identifies the account at the CA once it is registered.
	URI string
	// EABKid and EABHMAC bind the account to one at the CA, which ZeroSSL
	// requires: the key ID and the base64url HMAC key it hands out.
	EABKid  string
	EABHMAC string
}

// user is the account as lego asks for it.
type user struct{ account *Account }

func (u user) GetEmail() string { return u.account.Email }

func (u user) GetRegistration() *registration.Resource {
	if u.account.URI == "" {
		return nil
	}
	return &registration.Resource{URI: u.account.URI}
}

func (u user) GetPrivateKey() crypto.PrivateKey { return u.account.Key }

// NewKey makes the key of an account or a certificate: ECDSA P-256, which
// every CA and client takes.
func NewKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// EncodeKey writes a key as PEM.
func EncodeKey(key crypto.Signer) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// DecodeKey reads a key that EncodeKey wrote.
func DecodeKey(text string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("the account key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("the account key cannot sign")
	}
	return signer, nil
}

// UserAgent is sent to the CA with every request.
var UserAgent = "PocketCloud"

func newClient(account *Account, httpClient *http.Client) (*lego.Client, error) {
	if account.Key == nil {
		return nil, errors.New("the account has no key")
	}
	config := lego.NewConfig(user{account})
	config.CADirURL = account.DirectoryURL
	config.UserAgent = UserAgent
	config.Certificate.KeyType = certcrypto.EC256
	config.Certificate.Timeout = 90 * time.Second
	if httpClient != nil {
		// lego wraps the client's transport in place, so each client gets
		// a copy: a shared one would be changed under the issuances that
		// use it, and wrapped again by each new one
		copied := *httpClient
		config.HTTPClient = &copied
	}
	return lego.NewClient(config)
}

// Register creates the account at its CA, agreeing to the CA's terms, and
// returns its URI. A key the CA knows already gets its existing account.
func Register(account *Account, httpClient *http.Client) (string, error) {
	client, err := newClient(account, httpClient)
	if err != nil {
		return "", err
	}
	var resource *registration.Resource
	if account.EABKid != "" {
		resource, err = client.Registration.RegisterWithExternalAccountBinding(registration.RegisterEABOptions{
			TermsOfServiceAgreed: true, Kid: account.EABKid, HmacEncoded: account.EABHMAC,
		})
	} else {
		resource, err = client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
	}
	if err != nil {
		return "", err
	}
	return resource.URI, nil
}

// Request is one certificate to issue.
type Request struct {
	Account *Account
	Names   []string
	// HTTP01 or DNS01 answers the challenges; exactly one of them is set.
	HTTP01 challenge.Provider
	DNS01  challenge.Provider
	// Propagated tells whether the CA can see the DNS-01 answer for a name
	// yet; it is asked until it says yes or the provider's timeout passes.
	Propagated func(domain, fqdn, value string) (bool, error)
	HTTPClient *http.Client
}

// Certificate is what the CA issued.
type Certificate struct {
	ChainPEM  []byte
	KeyPEM    []byte
	NotBefore time.Time
	NotAfter  time.Time
	Subject   string
	Issuer    string
	// AccountURI is the account's URI, which changes when the CA had
	// forgotten the account and it was registered again.
	AccountURI string
}

// Obtain issues a certificate for the names with a new key. An account the CA
// does not know any more, as after a reset of a staging CA, is registered
// again once.
func Obtain(req Request) (*Certificate, error) {
	if len(req.Names) == 0 {
		return nil, errors.New("a certificate needs at least one name")
	}
	if (req.HTTP01 == nil) == (req.DNS01 == nil) {
		return nil, errors.New("a certificate needs exactly one kind of challenge")
	}
	if req.Account.URI == "" {
		uri, err := Register(req.Account, req.HTTPClient)
		if err != nil {
			return nil, err
		}
		req.Account.URI = uri
	}

	cert, err := obtain(req)
	if isProblem(err, "accountDoesNotExist") {
		uri, regErr := Register(&Account{
			DirectoryURL: req.Account.DirectoryURL, Email: req.Account.Email, Key: req.Account.Key,
			EABKid: req.Account.EABKid, EABHMAC: req.Account.EABHMAC,
		}, req.HTTPClient)
		if regErr != nil {
			return nil, regErr
		}
		req.Account.URI = uri
		cert, err = obtain(req)
	}
	if err != nil {
		return nil, err
	}
	cert.AccountURI = req.Account.URI
	return cert, nil
}

func obtain(req Request) (*Certificate, error) {
	client, err := newClient(req.Account, req.HTTPClient)
	if err != nil {
		return nil, err
	}
	if req.HTTP01 != nil {
		if err := client.Challenge.SetHTTP01Provider(req.HTTP01); err != nil {
			return nil, err
		}
	} else {
		options := []dns01.ChallengeOption{}
		if req.Propagated != nil {
			options = append(options, dns01.WrapPreCheck(func(domain, fqdn, value string, _ dns01.PreCheckFunc) (bool, error) {
				return req.Propagated(domain, fqdn, value)
			}))
		}
		if err := client.Challenge.SetDNS01Provider(req.DNS01, options...); err != nil {
			return nil, err
		}
	}

	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	resource, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: req.Names, PrivateKey: key, Bundle: true})
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(resource.Certificate)
	if block == nil {
		return nil, errors.New("the CA's answer holds no certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	keyPEM, err := EncodeKey(key)
	if err != nil {
		return nil, err
	}
	return &Certificate{
		ChainPEM:  resource.Certificate,
		KeyPEM:    []byte(keyPEM),
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Subject:   leaf.Subject.String(),
		Issuer:    leaf.Issuer.String(),
	}, nil
}

// DNSAnswer is the TXT record that answers a DNS-01 challenge.
type DNSAnswer struct {
	// FQDN is _acme-challenge.<name>., with the final dot.
	FQDN  string
	Value string
}

// ChallengeInfo is the answer to a DNS-01 challenge for a name, as lego
// passes it to a provider: the name without a wildcard's "*.".
func ChallengeInfo(domain, keyAuth string) DNSAnswer {
	info := dns01.GetChallengeInfo(domain, keyAuth)
	return DNSAnswer{FQDN: info.FQDN, Value: info.Value}
}

const problemPrefix = "urn:ietf:params:acme:error:"

// isProblem tells whether an error holds an ACME problem of a type, such as
// "rateLimited".
func isProblem(err error, kind string) bool {
	var problem *legoacme.ProblemDetails
	return errors.As(err, &problem) && problem.Type == problemPrefix+kind
}

// RateLimited tells whether the CA refused because of its rate limits.
func RateLimited(err error) bool { return isProblem(err, "rateLimited") }

// Explain turns what lego returns into what the CA said, one line per name
// that failed, without lego's own framing.
func Explain(err error) string {
	if err == nil {
		return ""
	}
	lines := explainTree(err)
	// lego collects them in maps
	slices.Sort(lines)
	return strings.Join(slices.Compact(lines), "\n")
}

// explainTree finds the error of each name in what lego returns: the
// resolver keeps them in a map by name; the certifier joins them, as
// "name: error", and nests the joins.
func explainTree(err error) []string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if v := reflect.ValueOf(e); v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String {
			lines := []string{}
			for _, key := range v.MapKeys() {
				if inner, ok := v.MapIndex(key).Interface().(error); ok {
					lines = append(lines, key.String()+": "+explainOne(inner))
				}
			}
			return lines
		}
		if joined, ok := e.(interface{ Unwrap() []error }); ok {
			lines := []string{}
			for _, part := range joined.Unwrap() {
				if _, nested := part.(interface{ Unwrap() []error }); nested {
					lines = append(lines, explainTree(part)...)
					continue
				}
				lines = append(lines, explainName(part))
			}
			return lines
		}
	}
	return []string{explainOne(err)}
}

// explainName explains the error of one name, which lego wraps as
// "name: error".
func explainName(err error) string {
	inner := errors.Unwrap(err)
	if inner == nil {
		return explainOne(err)
	}
	name := strings.TrimSuffix(err.Error(), ": "+inner.Error())
	if name == err.Error() {
		return explainOne(err)
	}
	return name + ": " + explainOne(inner)
}

// legoFraming is what lego puts before the cause of an error.
var legoFraming = regexp.MustCompile(`^(\[[^\]]*\] )?(acme: )?(error: )?(error presenting token: |error cleaning up: )?`)

func explainOne(err error) string {
	var problem *legoacme.ProblemDetails
	if errors.As(err, &problem) {
		return describe(problem)
	}
	return legoFraming.ReplaceAllString(err.Error(), "")
}

// describe is a problem as a person reads it: the CA's own words, and its
// type when that adds something.
func describe(problem *legoacme.ProblemDetails) string {
	kind := strings.TrimPrefix(problem.Type, problemPrefix)
	detail := strings.TrimSpace(problem.Detail)
	for _, sub := range problem.SubProblems {
		if sub.Detail != "" && !strings.Contains(detail, sub.Detail) {
			detail += "; " + sub.Identifier.Value + ": " + sub.Detail
		}
	}
	switch {
	case detail == "" && kind == "":
		return fmt.Sprintf("the CA answered %d", problem.HTTPStatus)
	case detail == "":
		return "the CA answered " + kind
	case kind == "" || strings.Contains(detail, problem.Type):
		return detail
	default:
		return detail + " (" + kind + ")"
	}
}
