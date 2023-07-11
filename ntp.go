// Copyright 2015-2023 Brett Vickers.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package ntp provides an implementation of a Simple NTP (SNTP) client
// capable of querying the current time from a remote NTP server.  See
// RFC5905 (https://tools.ietf.org/html/rfc5905) for more details.
//
// This approach grew out of a go-nuts post by Michael Hofmann:
// https://groups.google.com/forum/?fromgroups#!topic/golang-nuts/FlcdMU5fkLQ
package ntp

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/ipv4"
)

var (
	ErrAuthFailed             = errors.New("authentication failed")
	ErrInvalidAuthKey         = errors.New("invalid authentication key")
	ErrInvalidDispersion      = errors.New("invalid dispersion in response")
	ErrInvalidLeapSecond      = errors.New("invalid leap second in response")
	ErrInvalidMode            = errors.New("invalid mode in response")
	ErrInvalidProtocolVersion = errors.New("invalid protocol version requested")
	ErrInvalidStratum         = errors.New("invalid stratum in response")
	ErrInvalidTime            = errors.New("invalid time reported")
	ErrInvalidTransmitTime    = errors.New("invalid transmit time in response")
	ErrKissOfDeath            = errors.New("kiss of death received")
	ErrServerClockFreshness   = errors.New("server clock not fresh")
	ErrServerResponseMismatch = errors.New("server response didn't match request")
	ErrServerTickedBackwards  = errors.New("server clock ticked backwards")
)

// The LeapIndicator is used to warn if a leap second should be inserted
// or deleted in the last minute of the current month.
type LeapIndicator uint8

const (
	// LeapNoWarning indicates no impending leap second.
	LeapNoWarning LeapIndicator = 0

	// LeapAddSecond indicates the last minute of the day has 61 seconds.
	LeapAddSecond = 1

	// LeapDelSecond indicates the last minute of the day has 59 seconds.
	LeapDelSecond = 2

	// LeapNotInSync indicates an unsynchronized leap second.
	LeapNotInSync = 3
)

// Internal constants
const (
	defaultNtpVersion = 4
	defaultNtpPort    = 123
	nanoPerSec        = 1000000000
	maxStratum        = 16
	defaultTimeout    = 5 * time.Second
	maxPollInterval   = (1 << 17) * time.Second
	maxDispersion     = 16 * time.Second
)

// Internal variables
var (
	ntpEpoch = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
)

type mode uint8

// NTP modes. This package uses only client mode.
const (
	reserved mode = 0 + iota
	symmetricActive
	symmetricPassive
	client
	server
	broadcast
	controlMessage
	reservedPrivate
)

// An ntpTime is a 64-bit fixed-point (Q32.32) representation of the number of
// seconds elapsed.
type ntpTime uint64

// Duration interprets the fixed-point ntpTime as a number of elapsed seconds
// and returns the corresponding time.Duration value.
func (t ntpTime) Duration() time.Duration {
	sec := (t >> 32) * nanoPerSec
	frac := (t & 0xffffffff) * nanoPerSec
	nsec := frac >> 32
	if uint32(frac) >= 0x80000000 {
		nsec++
	}
	return time.Duration(sec + nsec)
}

// Time interprets the fixed-point ntpTime as an absolute time and returns
// the corresponding time.Time value.
func (t ntpTime) Time() time.Time {
	return ntpEpoch.Add(t.Duration())
}

// toNtpTime converts the time.Time value t into its 64-bit fixed-point
// ntpTime representation.
func toNtpTime(t time.Time) ntpTime {
	nsec := uint64(t.Sub(ntpEpoch))
	sec := nsec / nanoPerSec
	nsec = uint64(nsec-sec*nanoPerSec) << 32
	frac := uint64(nsec / nanoPerSec)
	if nsec%nanoPerSec >= nanoPerSec/2 {
		frac++
	}
	return ntpTime(sec<<32 | frac)
}

// An ntpTimeShort is a 32-bit fixed-point (Q16.16) representation of the
// number of seconds elapsed.
type ntpTimeShort uint32

// Duration interprets the fixed-point ntpTimeShort as a number of elapsed
// seconds and returns the corresponding time.Duration value.
func (t ntpTimeShort) Duration() time.Duration {
	sec := uint64(t>>16) * nanoPerSec
	frac := uint64(t&0xffff) * nanoPerSec
	nsec := frac >> 16
	if uint16(frac) >= 0x8000 {
		nsec++
	}
	return time.Duration(sec + nsec)
}

// header is an internal representation of an NTP packet header.
type header struct {
	LiVnMode       uint8 // Leap Indicator (2) + Version (3) + Mode (3)
	Stratum        uint8
	Poll           int8
	Precision      int8
	RootDelay      ntpTimeShort
	RootDispersion ntpTimeShort
	ReferenceID    uint32 // KoD code if Stratum == 0
	ReferenceTime  ntpTime
	OriginTime     ntpTime
	ReceiveTime    ntpTime
	TransmitTime   ntpTime
}

// setVersion sets the NTP protocol version on the header.
func (h *header) setVersion(v int) {
	h.LiVnMode = (h.LiVnMode & 0xc7) | uint8(v)<<3
}

// setMode sets the NTP protocol mode on the header.
func (h *header) setMode(md mode) {
	h.LiVnMode = (h.LiVnMode & 0xf8) | uint8(md)
}

// setLeap modifies the leap indicator on the header.
func (h *header) setLeap(li LeapIndicator) {
	h.LiVnMode = (h.LiVnMode & 0x3f) | uint8(li)<<6
}

// getMode returns the mode value in the header.
func (h *header) getMode() mode {
	return mode(h.LiVnMode & 0x07)
}

// getLeap returns the leap indicator on the header.
func (h *header) getLeap() LeapIndicator {
	return LeapIndicator((h.LiVnMode >> 6) & 0x03)
}

// An Extension adds custom behaviors capable of modifying NTP packets before
// being sent to the server and processing packets after being received by the
// server.
type Extension interface {
	// ProcessQuery is called when the client is about to send a query to the
	// NTP server. The buffer contains the NTP header. It may also contain
	// extension fields added by extensions processed prior to this one.
	ProcessQuery(buf *bytes.Buffer) error

	// ProcessResponse is called after the client has received the server's
	// NTP response. The buffer contains the entire message returned by the
	// server.
	ProcessResponse(buf []byte) error
}

// QueryOptions contains configurable options used by the QueryWithOptions
// function.
type QueryOptions struct {
	// Timeout determines how long the client waits for a response from the
	// server before failing with a timeout error. Defaults to 5 seconds.
	Timeout time.Duration

	// Version of the NTP protocol to use. Defaults to 4.
	Version int

	// LocalAddress contains the local IP address to use when creating a
	// connection to the remote NTP server. This may be useful when the local
	// system has more than one IP address. This address should not contain
	// a port number.
	LocalAddress string

	// Port indicates the port used to reach the remote NTP server.
	//
	// DEPRECATED. Embed the port number in the query address string instead.
	Port int

	// TTL specifies the maximum number of IP hops before the query datagram
	// is dropped by the network. Defaults to the local system's default value.
	TTL int

	// Auth contains the settings used to configure NTP symmetric key
	// authentication. See RFC5905 for further details.
	Auth AuthOptions

	// Extensions may be added to modify NTP queries before they are
	// transmitted and to process NTP responses after they arrive.
	Extensions []Extension

	// Dialer is a callback used to override the default UDP network dialer.
	// The localAddress is directly copied from the LocalAddress field
	// specified in QueryOptions. It may be the empty string or a host address
	// (without port number). The remoteAddress is the "host:port" string
	// derived from the first parameter to QueryWithOptions.  The
	// remoteAddress is guaranteed to include a port number.
	Dialer func(localAddress, remoteAddress string) (net.Conn, error)

	// Dial is a callback used to override the default UDP network dialer.
	//
	// DEPRECATED. Use Dialer instead.
	Dial func(laddr string, lport int, raddr string, rport int) (net.Conn, error)
}

// A Response contains time data, some of which is returned by the NTP server
// and some of which is calculated by this client.
type Response struct {
	// Time is the transmit time reported by the server just before it
	// responded to the client's NTP query.
	Time time.Time

	// ClockOffset is the estimated offset of the local system clock relative
	// to the server's clock. Add this value to subsequent local system time
	// measurements in order to obtain a more accurate time.
	ClockOffset time.Duration

	// RTT is the measured round-trip-time delay estimate between the client
	// and the server.
	RTT time.Duration

	// Precision is the reported precision of the server's clock.
	Precision time.Duration

	// Stratum is the "stratum level" of the server. The smaller the number,
	// the closer the server is to the reference clock. Stratum 1 servers are
	// attached directly to the reference clock. A stratum value of 0
	// indicates the "kiss of death," which typically occurs when the client
	// issues too many requests to the server in a short period of time.
	Stratum uint8

	// ReferenceID is a 32-bit identifier identifying the server or
	// reference clock.
	ReferenceID uint32

	// ReferenceTime is the time when the server's system clock was last
	// set or corrected.
	ReferenceTime time.Time

	// RootDelay is the server's estimated aggregate round-trip-time delay to
	// the stratum 1 server.
	RootDelay time.Duration

	// RootDispersion is the server's estimated maximum measurement error
	// relative to the stratum 1 server.
	RootDispersion time.Duration

	// RootDistance is an estimate of the total synchronization distance
	// between the client and the stratum 1 server.
	RootDistance time.Duration

	// Leap indicates whether a leap second should be added or removed from
	// the current month's last minute.
	Leap LeapIndicator

	// MinError is a lower bound on the error between the client and server
	// clocks. When the client and server are not synchronized to the same
	// clock, the reported timestamps may appear to violate the principle of
	// causality. In other words, the NTP server's response may indicate
	// that a message was received before it was sent. In such cases, the
	// minimum error may be useful.
	MinError time.Duration

	// KissCode is a 4-character string describing the reason for a
	// "kiss of death" response (stratum = 0). For a list of standard kiss
	// codes, see https://tools.ietf.org/html/rfc5905#section-7.4.
	KissCode string

	// Poll is the maximum interval between successive NTP query messages to
	// the server.
	Poll time.Duration

	authErr error
}

// Validate checks if the response is valid for the purposes of time
// synchronization.
func (r *Response) Validate() error {
	// Forward authentication errors.
	if r.authErr != nil {
		return r.authErr
	}

	// Handle invalid stratum values.
	if r.Stratum == 0 {
		return ErrKissOfDeath
	}
	if r.Stratum >= maxStratum {
		return ErrInvalidStratum
	}

	// Estimate the "freshness" of the time. If it exceeds the maximum
	// polling interval (~36 hours), then it cannot be considered "fresh".
	freshness := r.Time.Sub(r.ReferenceTime)
	if freshness > maxPollInterval {
		return ErrServerClockFreshness
	}

	// Calculate the peer synchronization distance, lambda:
	//  	lambda := RootDelay/2 + RootDispersion
	// If this value exceeds MAXDISP (16s), then the time is not suitable
	// for synchronization purposes.
	// https://tools.ietf.org/html/rfc5905#appendix-A.5.1.1.
	lambda := r.RootDelay/2 + r.RootDispersion
	if lambda > maxDispersion {
		return ErrInvalidDispersion
	}

	// If the server's transmit time is before its reference time, the
	// response is invalid.
	if r.Time.Before(r.ReferenceTime) {
		return ErrInvalidTime
	}

	// Handle invalid leap second indicator.
	if r.Leap == LeapNotInSync {
		return ErrInvalidLeapSecond
	}

	// nil means the response is valid.
	return nil
}

// Query requests time data from a remote NTP server. The server address is of
// the form "host", "host:port", "host%zone:port", "[host]:port" or
// "[host%zone]:port". If no port is included, NTP default port 123 is used.
// The response contains information from which an accurate local time can be
// determined.
func Query(address string) (*Response, error) {
	return QueryWithOptions(address, QueryOptions{})
}

// QueryWithOptions performs the same function as Query but allows for the
// customization of certain query behaviors. See the comment for Query for
// more information.
func QueryWithOptions(address string, opt QueryOptions) (*Response, error) {
	h, now, err := getTime(address, &opt)
	if err != nil && err != ErrAuthFailed {
		return nil, err
	}

	return generateResponse(h, now, err), nil
}

// Time returns the current local time using information returned from the
// remote NTP server. The server address is of the form "host", "host:port",
// "host%zone:port", "[host]:port" or "[host%zone]:port". If no port is
// included, NTP default port 123 is used. On error, Time returns the local
// system time.
func Time(address string) (time.Time, error) {
	r, err := Query(address)
	if err != nil {
		return time.Now(), err
	}

	err = r.Validate()
	if err != nil {
		return time.Now(), err
	}

	// Use the response's clock offset to calculate an accurate time.
	return time.Now().Add(r.ClockOffset), nil
}

// getTime performs the NTP server query and returns the response header
// along with the local system time it was received.
func getTime(address string, opt *QueryOptions) (*header, ntpTime, error) {
	if opt.Timeout == 0 {
		opt.Timeout = defaultTimeout
	}
	if opt.Version == 0 {
		opt.Version = defaultNtpVersion
	}
	if opt.Version < 2 || opt.Version > 4 {
		return nil, 0, ErrInvalidProtocolVersion
	}
	if opt.Port == 0 {
		opt.Port = defaultNtpPort
	}
	if opt.Dial != nil {
		// wrapper for the deprecated Dial callback.
		opt.Dialer = func(la, ra string) (net.Conn, error) {
			return dialWrapper(la, ra, opt.Dial)
		}
	}
	if opt.Dialer == nil {
		opt.Dialer = defaultDialer
	}

	// Compose a remote "host:port" address string if the address string
	// doesn't already contain a port.
	remoteAddress := address
	_, _, err := net.SplitHostPort(address)
	if err != nil {
		if strings.Contains(err.Error(), "missing port") {
			remoteAddress = net.JoinHostPort(address, strconv.Itoa(opt.Port))
		} else {
			return nil, 0, err
		}
	}

	// Connect to the remote server.
	con, err := opt.Dialer(opt.LocalAddress, remoteAddress)
	if err != nil {
		return nil, 0, err
	}
	defer con.Close()

	// Set a TTL for the packet if requested.
	if opt.TTL != 0 {
		ipcon := ipv4.NewConn(con)
		err = ipcon.SetTTL(opt.TTL)
		if err != nil {
			return nil, 0, err
		}
	}

	// If using symmetric key authentication, decode and validate the auth key
	// string.
	var decodedAuthKey []byte
	if opt.Auth.Type != AuthNone {
		decodedAuthKey, err = decodeAuthKey(opt.Auth)
		if err != nil {
			return nil, 0, err
		}
	}

	// Set a timeout on the connection.
	con.SetDeadline(time.Now().Add(opt.Timeout))

	// Allocate a buffer big enough to hold an entire response datagram.
	recvBuf := make([]byte, 8192)
	recvHdr := new(header)

	// Allocate the query message header.
	xmitHdr := new(header)
	xmitHdr.setMode(client)
	xmitHdr.setVersion(opt.Version)
	xmitHdr.setLeap(LeapNoWarning)
	xmitHdr.Precision = 0x20

	// To help prevent spoofing and client fingerprinting, use a
	// cryptographically random 64-bit value for the TransmitTime. See:
	// https://www.ietf.org/archive/id/draft-ietf-ntp-data-minimization-04.txt
	bits := make([]byte, 8)
	_, err = rand.Read(bits)
	if err != nil {
		return nil, 0, err
	}
	xmitHdr.TransmitTime = ntpTime(binary.BigEndian.Uint64(bits))

	// Write the query header to a transmit buffer.
	var xmitBuf bytes.Buffer
	binary.Write(&xmitBuf, binary.BigEndian, xmitHdr)

	// Allow extensions to process the query and add to the transmit buffer.
	for _, e := range opt.Extensions {
		err = e.ProcessQuery(&xmitBuf)
		if err != nil {
			return nil, 0, err
		}
	}

	// Append an authentication MAC if requested.
	if opt.Auth.Type != AuthNone {
		appendMAC(&xmitBuf, opt.Auth, decodedAuthKey)
	}

	// Transmit the query and keep track of when it was transmitted.
	xmitTime := time.Now()
	_, err = con.Write(xmitBuf.Bytes())
	if err != nil {
		return nil, 0, err
	}

	// Receive the response.
	recvBytes, err := con.Read(recvBuf)
	if err != nil {
		return nil, 0, err
	}

	// Keep track of the time the response was received. As of go 1.9, the
	// time package uses a monotonic clock, so delta will never be less than
	// zero for go version 1.9 or higher.
	delta := time.Since(xmitTime)
	if delta < 0 {
		delta = 0
	}
	recvTime := xmitTime.Add(delta)

	// Deserialize the response header.
	recvBuf = recvBuf[:recvBytes]
	recvReader := bytes.NewReader(recvBuf)
	err = binary.Read(recvReader, binary.BigEndian, recvHdr)
	if err != nil {
		return nil, 0, err
	}

	// Allow extensions to process the response.
	for i := len(opt.Extensions) - 1; i >= 0; i-- {
		err = opt.Extensions[i].ProcessResponse(recvBuf)
		if err != nil {
			return nil, 0, err
		}
	}

	// Perform authentication of the server response.
	var authErr error
	if opt.Auth.Type != AuthNone {
		authErr = verifyMAC(recvBuf, opt.Auth, decodedAuthKey)
	}

	// Check for invalid fields.
	if recvHdr.getMode() != server {
		return nil, 0, ErrInvalidMode
	}
	if recvHdr.TransmitTime == ntpTime(0) {
		return nil, 0, ErrInvalidTransmitTime
	}
	if recvHdr.OriginTime != xmitHdr.TransmitTime {
		return nil, 0, ErrServerResponseMismatch
	}
	if recvHdr.ReceiveTime > recvHdr.TransmitTime {
		return nil, 0, ErrServerTickedBackwards
	}

	// Correct the received message's origin time using the actual
	// transmit time.
	recvHdr.OriginTime = toNtpTime(xmitTime)

	return recvHdr, toNtpTime(recvTime), authErr
}

// defaultDialer provides a UDP dialer based on Go's built-in net stack.
func defaultDialer(localAddress, remoteAddress string) (net.Conn, error) {
	var laddr *net.UDPAddr
	if localAddress != "" {
		var err error
		laddr, err = net.ResolveUDPAddr("udp", net.JoinHostPort(localAddress, "0"))
		if err != nil {
			return nil, err
		}
	}

	raddr, err := net.ResolveUDPAddr("udp", remoteAddress)
	if err != nil {
		return nil, err
	}

	return net.DialUDP("udp", laddr, raddr)
}

// dialWrapper is used to wrap the deprecated Dial callback in QueryOptions.
func dialWrapper(la, ra string,
	dial func(la string, lp int, ra string, rp int) (net.Conn, error)) (net.Conn, error) {
	rhost, rport, err := net.SplitHostPort(ra)
	if err != nil {
		return nil, err
	}

	rportValue, err := strconv.Atoi(rport)
	if err != nil {
		return nil, err
	}

	return dial(la, 0, rhost, rportValue)
}

// generateResponse processes NTP header fields along with the its receive
// time to generate a Response record.
func generateResponse(h *header, recvTime ntpTime, authErr error) *Response {
	r := &Response{
		Time:           h.TransmitTime.Time(),
		ClockOffset:    offset(h.OriginTime, h.ReceiveTime, h.TransmitTime, recvTime),
		RTT:            rtt(h.OriginTime, h.ReceiveTime, h.TransmitTime, recvTime),
		Precision:      toInterval(h.Precision),
		Stratum:        h.Stratum,
		ReferenceID:    h.ReferenceID,
		ReferenceTime:  h.ReferenceTime.Time(),
		RootDelay:      h.RootDelay.Duration(),
		RootDispersion: h.RootDispersion.Duration(),
		Leap:           h.getLeap(),
		MinError:       minError(h.OriginTime, h.ReceiveTime, h.TransmitTime, recvTime),
		Poll:           toInterval(h.Poll),
		authErr:        authErr,
	}

	// Calculate values depending on other calculated values
	r.RootDistance = rootDistance(r.RTT, r.RootDelay, r.RootDispersion)

	// If a kiss of death was received, interpret the reference ID as
	// a kiss code.
	if r.Stratum == 0 {
		r.KissCode = kissCode(r.ReferenceID)
	}

	return r
}

// The following helper functions calculate additional metadata about the
// timestamps received from an NTP server.  The timestamps returned by
// the server are given the following variable names:
//
//   org = Origin Timestamp (client send time)
//   rec = Receive Timestamp (server receive time)
//   xmt = Transmit Timestamp (server reply time)
//   dst = Destination Timestamp (client receive time)

func rtt(org, rec, xmt, dst ntpTime) time.Duration {
	// round trip delay time
	//   rtt = (dst-org) - (xmt-rec)
	a := dst.Time().Sub(org.Time())
	b := xmt.Time().Sub(rec.Time())
	rtt := a - b
	if rtt < 0 {
		rtt = 0
	}
	return rtt
}

func offset(org, rec, xmt, dst ntpTime) time.Duration {
	// local clock offset
	//   offset = ((rec-org) + (xmt-dst)) / 2
	a := rec.Time().Sub(org.Time())
	b := xmt.Time().Sub(dst.Time())
	return (a + b) / time.Duration(2)
}

func minError(org, rec, xmt, dst ntpTime) time.Duration {
	// Each NTP response contains two pairs of send/receive timestamps.
	// When either pair indicates a "causality violation", we calculate the
	// error as the difference in time between them. The minimum error is
	// the greater of the two causality violations.
	var error0, error1 ntpTime
	if org >= rec {
		error0 = org - rec
	}
	if xmt >= dst {
		error1 = xmt - dst
	}
	if error0 > error1 {
		return error0.Duration()
	}
	return error1.Duration()
}

func rootDistance(rtt, rootDelay, rootDisp time.Duration) time.Duration {
	// The root distance is:
	// 	the maximum error due to all causes of the local clock
	//	relative to the primary server. It is defined as half the
	//	total delay plus total dispersion plus peer jitter.
	//	(https://tools.ietf.org/html/rfc5905#appendix-A.5.5.2)
	//
	// In the reference implementation, it is calculated as follows:
	//	rootDist = max(MINDISP, rootDelay + rtt)/2 + rootDisp
	//			+ peerDisp + PHI * (uptime - peerUptime)
	//			+ peerJitter
	// For an SNTP client which sends only a single packet, most of these
	// terms are irrelevant and become 0.
	totalDelay := rtt + rootDelay
	return totalDelay/2 + rootDisp
}

func toInterval(t int8) time.Duration {
	switch {
	case t > 0:
		return time.Duration(uint64(time.Second) << uint(t))
	case t < 0:
		return time.Duration(uint64(time.Second) >> uint(-t))
	default:
		return time.Second
	}
}

func kissCode(id uint32) string {
	isPrintable := func(ch byte) bool { return ch >= 32 && ch <= 126 }

	b := []byte{
		byte(id >> 24),
		byte(id >> 16),
		byte(id >> 8),
		byte(id),
	}
	for _, ch := range b {
		if !isPrintable(ch) {
			return ""
		}
	}
	return string(b)
}
