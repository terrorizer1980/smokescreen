package smokescreen

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	log "github.com/sirupsen/logrus"
)

type Config struct {
	Ip                           string
	Port                         int
	CidrBlacklist                []net.IPNet
	CidrBlacklistExemptions      []net.IPNet
	ConnectTimeout               time.Duration
	ExitTimeout                  time.Duration
	MaintenanceFile              string
	StatsdClient                 *statsd.Client
	EgressAcl                    EgressAcl
	SupportProxyProtocol         bool
	TlsConfig                    *tls.Config
	CrlByAuthorityKeyId          map[string]*pkix.CertificateList
	RoleFromRequest              func(subject *http.Request) (string, error)
	clientCasBySubjectKeyId      map[string]*x509.Certificate
	AdditionalErrorMessageOnDeny string
	Log                          *log.Logger
	DisabledAclPolicyActions     []string

	hostExtractExpr *regexp.Regexp
}

type missingRoleError struct {
	error
}

func MissingRoleError(s string) error {
	return missingRoleError{errors.New(s)}
}

func IsMissingRoleError(err error) bool {
	_, ok := err.(missingRoleError)
	return ok
}

func parseRanges(rangeStrings []string) ([]net.IPNet, error) {
	outRanges := make([]net.IPNet, len(rangeStrings))
	for i, str := range rangeStrings {
		_, ipnet, err := net.ParseCIDR(str)
		if err != nil {
			return outRanges, err
		}
		outRanges[i] = *ipnet
	}
	return outRanges, nil
}

func (config *Config) SetDenyRanges(rangeStrings []string) error {
	var err error
	config.CidrBlacklist, err = parseRanges(rangeStrings)
	return err
}

func (config *Config) SetAllowRanges(rangeStrings []string) error {
	var err error
	config.CidrBlacklistExemptions, err = parseRanges(rangeStrings)
	return err
}

// RFC 5280,  4.2.1.1
type authKeyId struct {
	Id []byte `asn1:"optional,tag:0"`
}

func (config *Config) Init() error {
	var err error

	if config.CrlByAuthorityKeyId == nil {
		config.CrlByAuthorityKeyId = make(map[string]*pkix.CertificateList)
	}
	if config.clientCasBySubjectKeyId == nil {
		config.clientCasBySubjectKeyId = make(map[string]*x509.Certificate)
	}
	if config.Log == nil {
		config.Log = log.New()
	}

	config.hostExtractExpr, err = regexp.Compile("^([^:]*)(:\\d+)?$")
	if err != nil {
		return err
	}

	// Configure RoleFromRequest for default behavior. It is ultimately meant to be replaced by the user.
	if config.TlsConfig != nil && config.TlsConfig.ClientCAs != nil { // If client certs are set, pick the CN.
		config.RoleFromRequest = func(req *http.Request) (string, error) {
			fail := func(err error) (string, error) { return "", err }
			if len(req.TLS.PeerCertificates) == 0 {
				return fail(MissingRoleError("client did not provide certificate"))
			}
			return req.TLS.PeerCertificates[0].Subject.CommonName, nil
		}
	} else { // Use a custom header
		config.RoleFromRequest = func(req *http.Request) (string, error) {
			fail := func(err error) (string, error) { return "", err }
			idHeader := req.Header["X-Smokescreen-Role"]
			if len(idHeader) == 0 {
				return fail(MissingRoleError("client did not send 'X-Smokescreen-Role' header"))
			} else if len(idHeader) > 1 {
				return fail(MissingRoleError("client sent multiple 'X-Smokescreen-Role' headers"))
			}
			return idHeader[0], nil
		}
	}

	return nil
}

func (config *Config) SetupCrls(crlFiles []string) error {
	fail := func(err error) error { fmt.Print(err); return err }

	config.CrlByAuthorityKeyId = make(map[string]*pkix.CertificateList)
	config.clientCasBySubjectKeyId = make(map[string]*x509.Certificate)

	for _, crlFile := range crlFiles {
		crlBytes, err := ioutil.ReadFile(crlFile)
		if err != nil {
			return fail(err)
		}

		certList, err := x509.ParseCRL(crlBytes)
		if err != nil {
			log.Printf("Failed to parse CRL in '%s': %#v\n", crlFile, err)
		}

		// find the X509v3 Authority Key Identifier in the extensions (2.5.29.35)
		crlIssuerId := ""
		extensionOid := []int{2, 5, 29, 35}
		for _, v := range certList.TBSCertList.Extensions {
			if v.Id.Equal(extensionOid) { // Hurray, we found it
				// Boo, it's ASN.1.
				var crlAuthorityKey authKeyId
				_, err := asn1.Unmarshal(v.Value, &crlAuthorityKey)
				if err != nil {
					fmt.Printf("error: Failed to read AuthorityKey: %#v\n", err)
					continue
				}
				crlIssuerId = string(crlAuthorityKey.Id)
				break
			}
		}
		if crlIssuerId == "" {
			log.Print(fmt.Errorf("error: CRL from '%s' has no Authority Key Identifier: ignoring it\n", crlFile))
			continue
		}

		// Make sure we have a CA for this CRL or warn
		caCert, ok := config.clientCasBySubjectKeyId[crlIssuerId]

		if !ok {
			log.Printf("warn: CRL loaded for issuer '%s' but no such CA loaded: ignoring it\n", hex.EncodeToString([]byte(crlIssuerId)))
			fmt.Printf("%#v loaded certs\n", len(config.clientCasBySubjectKeyId))
			continue
		}

		// At this point, we have the CA certificate and the CRL. All that's left before evicting the CRL we currently trust is to verify the new CRL's signature
		err = caCert.CheckCRLSignature(certList)
		if err != nil {
			fmt.Printf("error: Could not trust CRL. Error during signature check: %#v\n", err)
			continue
		}

		// At this point, we have a new CRL which we trust. Let's evict the old one.
		config.CrlByAuthorityKeyId[crlIssuerId] = certList
		fmt.Printf("info: Loaded CRL for Authority ID '%s'\n", hex.EncodeToString([]byte(crlIssuerId)))
	}

	// Verify that all CAs loaded have a CRL
	for k, _ := range config.clientCasBySubjectKeyId {
		_, ok := config.CrlByAuthorityKeyId[k]
		if !ok {
			fmt.Printf("warn: no CRL loaded for Authority ID '%s'\n", hex.EncodeToString([]byte(k)))
		}
	}
	return nil
}

func (config *Config) SetupStatsdWithNamespace(addr, namespace string) error {
	if addr == "" {
		config.StatsdClient = nil
		return nil
	}

	client, err := statsd.New(addr)
	if err != nil {
		return err
	}

	config.StatsdClient = client

	config.StatsdClient.Namespace = namespace

	return nil
}

func (config *Config) SetupStatsd(addr string) error {
	return config.SetupStatsdWithNamespace(addr, DefaultStatsdNamespace)
}

func (config *Config) SetupEgressAcl(aclFile string) error {
	if aclFile == "" {
		config.EgressAcl = nil
		return nil
	}

	log.Printf("Loading egress ACL from %s", aclFile)
	egressAcl, err := LoadYamlAclFromFilePath(config, aclFile)
	if err != nil {
		log.Print(err)
		return err
	}
	config.EgressAcl = egressAcl

	return nil
}

func addCertsFromFile(config *Config, pool *x509.CertPool, fileName string) error {
	data, err := ioutil.ReadFile(fileName)

	//TODO this is a bit awkward
	config.populateClientCaMap(data)

	if err != nil {
		return err
	}
	ok := pool.AppendCertsFromPEM(data)
	if !ok {
		return fmt.Errorf("Failed to load any certificates from file '%s'", fileName)
	}
	return nil
}

// certFile and keyFile may be the same file containing concatenated PEM blocks
func (config *Config) SetupTls(certFile, keyFile string, clientCAFiles []string) error {
	if certFile == "" || keyFile == "" {
		return errors.New("both certificate and key files must be specified to set up TLS")
	}

	serverCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}

	clientAuth := tls.NoClientCert
	clientCAs := x509.NewCertPool()

	if len(clientCAFiles) != 0 {
		clientAuth = tls.VerifyClientCertIfGiven
		for _, caFile := range clientCAFiles {
			err = addCertsFromFile(config, clientCAs, caFile)
			if err != nil {
				return err
			}
		}
	}

		config.TlsConfig = &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			ClientAuth: clientAuth,
			ClientCAs: clientCAs,
		}

	return nil
}

func (config *Config) populateClientCaMap(pemCerts []byte) (ok bool) {

	for len(pemCerts) > 0 {
		var block *pem.Block
		block, pemCerts = pem.Decode(pemCerts)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		fmt.Printf("info: Loaded CA with Authority ID '%s'\n", hex.EncodeToString(cert.SubjectKeyId))
		config.clientCasBySubjectKeyId[string(cert.SubjectKeyId)] = cert
		ok = true
	}
	return
}
