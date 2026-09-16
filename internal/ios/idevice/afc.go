package idevice

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"time"
)

// AFC ("Apple File Conduit") moves files in and out of the device's media
// sandbox. It does not use plist framing: every message is a 40 byte header
// followed by an operation-specific body.
const (
	afcMagic       = "CFA6LPAA"
	afcHeaderSize  = 40
	afcServiceName = "com.apple.afc"

	// afcChunk is the payload size per FILE_WRITE. usbmuxd copies through a
	// USB bulk pipe; 64 KiB keeps the pipe busy without oversized allocations.
	afcChunk = 64 * 1024
)

// AFC operation codes.
const (
	afcOpStatus      uint64 = 0x00000001
	afcOpData        uint64 = 0x00000002
	afcOpRemovePath  uint64 = 0x00000008
	afcOpMakeDir     uint64 = 0x00000009
	afcOpGetFileInfo uint64 = 0x0000000A
	afcOpFileOpen    uint64 = 0x0000000D
	afcOpFileOpenRes uint64 = 0x0000000E
	afcOpFileWrite   uint64 = 0x00000010
	afcOpFileClose   uint64 = 0x00000014
)

// AFC file open modes.
const (
	afcModeWrOnly uint64 = 0x00000003 // "w": create/truncate
)

// afcStatus is the AFC error enumeration.
type afcStatus uint64

const afcStatusSuccess afcStatus = 0

var afcStatusNames = map[afcStatus]string{
	0:  "success",
	1:  "unknown error",
	2:  "invalid operation header",
	3:  "no resources",
	4:  "read error",
	5:  "write error",
	6:  "unknown packet type",
	7:  "invalid argument",
	8:  "object not found",
	9:  "object is a directory",
	10: "permission denied",
	11: "service not connected",
	12: "operation timed out",
	13: "too much data",
	14: "end of data",
	15: "operation not supported",
	16: "object already exists",
	17: "object busy",
	18: "no space left on device",
	19: "operation would block",
	20: "io error",
	21: "operation interrupted",
	22: "operation in progress",
	23: "internal error",
	30: "mux error",
	31: "out of memory",
	32: "not enough data",
	33: "directory not empty",
}

func (s afcStatus) Error() string {
	if name, ok := afcStatusNames[s]; ok {
		return "afc: " + name
	}
	return fmt.Sprintf("afc: status %d", uint64(s))
}

func (s afcStatus) err() error {
	if s == afcStatusSuccess {
		return nil
	}
	return s
}

// afcClient is an AFC session on an already-open (and, when the device asked
// for it, already TLS-wrapped) service connection.
type afcClient struct {
	conn   net.Conn
	packet uint64
}

func newAFC(svc *service) *afcClient {
	return &afcClient{conn: svc.conn}
}

func (c *afcClient) Close() error { return c.conn.Close() }

// request sends one AFC packet. header carries the fixed-size operation
// arguments, payload the bulk bytes (only FILE_WRITE uses both).
func (c *afcClient) request(op uint64, header, payload []byte) error {
	entire := uint64(afcHeaderSize + len(header) + len(payload))
	this := uint64(afcHeaderSize + len(header))

	frame := make([]byte, afcHeaderSize+len(header))
	copy(frame, afcMagic)
	binary.LittleEndian.PutUint64(frame[8:], entire)
	binary.LittleEndian.PutUint64(frame[16:], this)
	binary.LittleEndian.PutUint64(frame[24:], c.packet)
	binary.LittleEndian.PutUint64(frame[32:], op)
	copy(frame[afcHeaderSize:], header)
	c.packet++

	if err := c.conn.SetWriteDeadline(time.Now().Add(ioTimeout)); err != nil {
		return err
	}
	if _, err := c.conn.Write(frame); err != nil {
		return fmt.Errorf("afc write header: %w", err)
	}
	if len(payload) > 0 {
		if _, err := c.conn.Write(payload); err != nil {
			return fmt.Errorf("afc write payload: %w", err)
		}
	}
	return nil
}

// response reads one AFC packet and returns its operation and body.
func (c *afcClient) response() (uint64, []byte, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(ioTimeout)); err != nil {
		return 0, nil, err
	}
	var hdr [afcHeaderSize]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return 0, nil, fmt.Errorf("afc read header: %w", err)
	}
	if string(hdr[:8]) != afcMagic {
		return 0, nil, fmt.Errorf("afc: bad magic %q", hdr[:8])
	}
	entire := binary.LittleEndian.Uint64(hdr[8:])
	this := binary.LittleEndian.Uint64(hdr[16:])
	op := binary.LittleEndian.Uint64(hdr[32:])
	if entire < afcHeaderSize || this < afcHeaderSize || this > entire {
		return 0, nil, fmt.Errorf("afc: inconsistent lengths entire=%d this=%d", entire, this)
	}
	if entire > afcHeaderSize+plistServiceMaxFrame {
		return 0, nil, fmt.Errorf("afc: oversized response %d", entire)
	}
	body := make([]byte, entire-afcHeaderSize)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		return 0, nil, fmt.Errorf("afc read body: %w", err)
	}
	return op, body, nil
}

// status reads a reply that must be a bare status.
func (c *afcClient) status(what string) error {
	op, body, err := c.response()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if op != afcOpStatus {
		return fmt.Errorf("%s: expected status packet, got operation %d", what, op)
	}
	if len(body) < 8 {
		return fmt.Errorf("%s: truncated status packet", what)
	}
	if err := afcStatus(binary.LittleEndian.Uint64(body)).err(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// MakeDir creates a directory, tolerating one that already exists.
func (c *afcClient) MakeDir(remote string) error {
	if err := c.request(afcOpMakeDir, afcPathBody(remote), nil); err != nil {
		return err
	}
	err := c.status("afc mkdir " + remote)
	if isAFCStatus(err, 16) {
		return nil
	}
	return err
}

// Remove deletes a path, tolerating a missing one.
func (c *afcClient) Remove(remote string) error {
	if err := c.request(afcOpRemovePath, afcPathBody(remote), nil); err != nil {
		return err
	}
	err := c.status("afc remove " + remote)
	if isAFCStatus(err, 8) {
		return nil
	}
	return err
}

// Exists reports whether a remote path is present.
func (c *afcClient) Exists(remote string) (bool, error) {
	if err := c.request(afcOpGetFileInfo, afcPathBody(remote), nil); err != nil {
		return false, err
	}
	op, body, err := c.response()
	if err != nil {
		return false, err
	}
	if op == afcOpData {
		return true, nil
	}
	if op == afcOpStatus && len(body) >= 8 {
		st := afcStatus(binary.LittleEndian.Uint64(body))
		if st == 8 {
			return false, nil
		}
		return false, fmt.Errorf("afc stat %s: %w", remote, st)
	}
	return false, fmt.Errorf("afc stat %s: unexpected operation %d", remote, op)
}

// open creates remote for writing and returns the device file handle.
func (c *afcClient) open(remote string, mode uint64) (uint64, error) {
	body := make([]byte, 8, 8+len(remote)+1)
	binary.LittleEndian.PutUint64(body, mode)
	body = append(body, afcPathBody(remote)...)
	if err := c.request(afcOpFileOpen, body, nil); err != nil {
		return 0, err
	}
	op, resp, err := c.response()
	if err != nil {
		return 0, fmt.Errorf("afc open %s: %w", remote, err)
	}
	if op == afcOpStatus && len(resp) >= 8 {
		return 0, fmt.Errorf("afc open %s: %w", remote, afcStatus(binary.LittleEndian.Uint64(resp)))
	}
	if op != afcOpFileOpenRes || len(resp) < 8 {
		return 0, fmt.Errorf("afc open %s: unexpected operation %d", remote, op)
	}
	return binary.LittleEndian.Uint64(resp), nil
}

func (c *afcClient) closeHandle(handle uint64) error {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint64(body, handle)
	if err := c.request(afcOpFileClose, body, nil); err != nil {
		return err
	}
	return c.status("afc close")
}

// WriteFile streams local into remote, reporting bytes written so far.
func (c *afcClient) WriteFile(local, remote string, progress func(sent, total int64)) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	total := info.Size()

	handle, err := c.open(remote, afcModeWrOnly)
	if err != nil {
		return err
	}

	buf := make([]byte, afcChunk)
	head := make([]byte, 8)
	binary.LittleEndian.PutUint64(head, handle)
	var sent int64
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if err := c.request(afcOpFileWrite, head, buf[:n]); err != nil {
				_ = c.closeHandle(handle)
				return err
			}
			if err := c.status("afc write " + remote); err != nil {
				_ = c.closeHandle(handle)
				return err
			}
			sent += int64(n)
			if progress != nil {
				progress(sent, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = c.closeHandle(handle)
			return readErr
		}
	}
	return c.closeHandle(handle)
}

// afcPathBody encodes a path the way AFC wants it: raw bytes, NUL terminated.
func afcPathBody(p string) []byte {
	body := make([]byte, 0, len(p)+1)
	body = append(body, p...)
	return append(body, 0)
}

func isAFCStatus(err error, want afcStatus) bool {
	if err == nil {
		return false
	}
	var st afcStatus
	for e := err; e != nil; {
		if s, ok := e.(afcStatus); ok {
			st = s
			return st == want
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// stagingDir is the directory installation_proxy reads packages from.
const stagingDir = "PublicStaging"

// uploadPackage copies ipa into PublicStaging and returns the device-relative
// path installation_proxy must be pointed at.
func (c *afcClient) uploadPackage(ipa string, progress func(sent, total int64)) (string, error) {
	if err := c.MakeDir(stagingDir); err != nil {
		return "", err
	}
	remote := path.Join(stagingDir, path.Base(ipa))
	if err := c.Remove(remote); err != nil {
		return "", err
	}
	if err := c.WriteFile(ipa, remote, progress); err != nil {
		return "", err
	}
	return remote, nil
}
