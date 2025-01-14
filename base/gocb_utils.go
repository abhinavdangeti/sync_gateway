package base

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/ioutil"
	"time"

	"github.com/couchbase/gocb"
)

// GoCBv2SecurityConfig returns a gocb.SecurityConfig to use when connecting given a CA Cert path.
func GoCBv2SecurityConfig(caCertPath string) (sc gocb.SecurityConfig, err error) {
	if caCertPath != "" {
		roots := x509.NewCertPool()
		cacert, err := ioutil.ReadFile(caCertPath)
		if err != nil {
			return sc, err
		}
		ok := roots.AppendCertsFromPEM(cacert)
		if !ok {
			return sc, errors.New("Invalid CA cert")
		}
		sc.TLSRootCAs = roots
	} else {
		sc.TLSSkipVerify = true
	}
	return sc, nil
}

// GoCBv2AuthenticatorConfig returns a gocb.Authenticator to use when connecting given a set of credentials.
func GoCBv2AuthenticatorConfig(username, password, certPath, keyPath string) (a gocb.Authenticator, isX509 bool, err error) {
	if certPath != "" && keyPath != "" {
		cert, certLoadErr := tls.LoadX509KeyPair(certPath, keyPath)
		if certLoadErr != nil {
			return nil, false, err
		}
		return gocb.CertificateAuthenticator{
			ClientCertificate: &cert,
		}, true, nil
	}

	return gocb.PasswordAuthenticator{
		Username: username,
		Password: password,
	}, false, nil
}

// GoCBv2TimeoutsConfig returns a gocb.TimeoutsConfig to use when connecting.
func GoCBv2TimeoutsConfig(bucketOpTimeout, viewQueryTimeout *time.Duration) (tc gocb.TimeoutsConfig) {
	if bucketOpTimeout != nil {
		tc.KVTimeout = *bucketOpTimeout
		tc.ManagementTimeout = *bucketOpTimeout
		tc.ConnectTimeout = *bucketOpTimeout
	}
	if viewQueryTimeout != nil {
		tc.QueryTimeout = *viewQueryTimeout
		tc.ViewTimeout = *viewQueryTimeout
	}
	return tc
}
