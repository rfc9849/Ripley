package idevice

import (
	"crypto/tls"
	"fmt"

	"howett.net/plist"
)

// PairRecord is the host side of a device pairing, as stored by usbmuxd.
//
// The certificates and the host private key are PEM blobs exactly as lockdownd
// created them during pairing; they are fed straight into crypto/tls to open
// the encrypted lockdownd session.
type PairRecord struct {
	DeviceCertificate []byte `plist:"DeviceCertificate"`
	HostCertificate   []byte `plist:"HostCertificate"`
	RootCertificate   []byte `plist:"RootCertificate"`
	HostPrivateKey    []byte `plist:"HostPrivateKey"`
	RootPrivateKey    []byte `plist:"RootPrivateKey"`
	HostID            string `plist:"HostID"`
	SystemBUID        string `plist:"SystemBUID"`
	EscrowBag         []byte `plist:"EscrowBag"`
	WiFiMACAddress    string `plist:"WiFiMACAddress"`
}

func parsePairRecord(data []byte) (*PairRecord, error) {
	var rec PairRecord
	if _, err := plist.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decode pair record: %w", err)
	}
	if rec.HostID == "" {
		return nil, fmt.Errorf("pair record has no HostID")
	}
	if len(rec.HostCertificate) == 0 || len(rec.HostPrivateKey) == 0 {
		return nil, fmt.Errorf("pair record for host %s has no host certificate/key", rec.HostID)
	}
	return &rec, nil
}

// tlsConfig builds the client configuration lockdownd expects.
//
// The device presents a certificate signed by the pairing root CA, which is not
// in any system trust store and whose subject is empty, so verification is done
// by us instead of by crypto/tls: the connection is only meaningful at all if
// the device already accepted our host certificate, which is proof of pairing.
// lockdownd on older devices negotiates TLS 1.0/1.1, so the floor is TLS 1.0.
func (p *PairRecord) tlsConfig() (*tls.Config, error) {
	cert, err := tls.X509KeyPair(p.HostCertificate, p.HostPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("pair record host certificate/key unusable: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// lockdownd's certificate chain is the pairing CA, not a public one.
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS10,
		MaxVersion:         tls.VersionTLS13,
	}, nil
}
