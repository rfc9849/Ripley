package idevice

import (
	"io"
	"time"
)

const syslogRelayService = "com.apple.syslog_relay"

// StreamSyslog copies the device's legacy syslog_relay byte stream to out until
// the service disconnects or the caller terminates the process. The relay is
// still exposed by lockdownd on current iOS releases and is useful for launch
// diagnostics when a distribution-signed app cannot be attached with LLDB.
func StreamSyslog(udid string, out io.Writer) error {
	s, err := openSession(udid)
	if err != nil {
		return err
	}
	defer s.Close()

	svc, err := s.ld.StartService(syslogRelayService)
	if err != nil {
		return err
	}
	defer svc.Close()

	// StartService may have established TLS with a temporary handshake deadline.
	// Syslog is a long-lived stream, so clear any remaining read deadline.
	if err := svc.conn.SetReadDeadline(time.Time{}); err != nil {
		return err
	}
	_, err = io.Copy(out, svc.conn)
	return err
}
