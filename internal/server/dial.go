package server

import (
	cryptoRand "crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"syscall"
	"time"

	"telix/internal/config"
	"telix/internal/dialer"
	"telix/internal/modem"
)

const dialCooldown = 3 * time.Second

func (s *Session) handleDial(number string) {
	// Enforce minimum time between dial attempts
	if time.Since(s.lastDial) < dialCooldown {
		s.sendResult(modem.ResultError)
		return
	}
	s.lastDial = time.Now()

	// Simulate waiting for dial tone (S6 register, default 2 seconds)
	dialToneWait := time.Duration(s.modem.Registers().GetDialToneWait()) * time.Second
	if dialToneWait > 0 {
		time.Sleep(dialToneWait)
	}

	// Look up number in phonebook
	entry := s.config.LookupNumber(number)
	if entry == nil {
		s.logger.InvalidNumber(number)
		s.sendIntercept()
		return
	}

	// Check required settings
	if !s.checkRequiredSettings(number, entry) {
		s.sendRings()
		s.sendGarbageConnect()
		return
	}

	// If the entry requires a password, prompt for it before dialing.
	// One attempt only — wrong password terminates the dial with NO CARRIER.
	if entry.Password != "" {
		if !s.promptPassword(number, entry.Password) {
			return
		}
	}

	// Attempt connection while ringing
	timeout := time.Duration(s.modem.Registers().GetConnectionTimeout()) * time.Second
	d := dialer.New(timeout, s.config.Dialer.ParsedNetworks())

	s.logger.ConnectionAttempt(number, "attempting")

	// Dial in background so we can ring while waiting
	type dialResult struct {
		conn net.Conn
		err  error
	}
	ch := make(chan dialResult, 1)
	go func() {
		conn, err := d.Dial(entry.Host, entry.Port)
		ch <- dialResult{conn, err}
	}()

	// Send 1-2 mandatory rings before checking the dial result,
	// so the user always hears ringing before a connection is picked up.
	mandatoryRings := 1 + rand.Intn(2)
	for i := 0; i < mandatoryRings; i++ {
		s.sendResult(modem.ResultRing)
		time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
	}

	// Continue ringing (0-2 more) while checking for dial completion
	extraRings := rand.Intn(3)
	for i := 0; i < extraRings; i++ {
		s.sendResult(modem.ResultRing)
		select {
		case res := <-ch:
			// Connection resolved during ringing
			if res.err != nil {
				s.logger.ConnectionAttempt(number, "failed")
				if isConnectionRefused(res.err) {
					s.sendResult(modem.ResultBusy)
				} else {
					s.sendResult(modem.ResultNoCarrier)
				}
				return
			}
			s.remoteMu.Lock()
			s.remoteConn = res.conn
			s.remoteMu.Unlock()
			s.modemHandshakePause()
			s.modem.SetState(modem.StateData)
			s.logger.ConnectionAttempt(number, "success")
			s.sendConnect(entry.RequiredSettings.Baud)
			return
		case <-time.After(time.Duration(2500+rand.Intn(1000)) * time.Millisecond):
			// Keep ringing
		}
	}

	// Done ringing, wait for dial to finish with a safety timeout
	var res dialResult
	select {
	case res = <-ch:
	case <-time.After(timeout + 5*time.Second):
		s.logger.ConnectionAttempt(number, "failed")
		s.sendResult(modem.ResultNoCarrier)
		return
	}
	if res.err != nil {
		s.logger.ConnectionAttempt(number, "failed")
		if isConnectionRefused(res.err) {
			s.sendResult(modem.ResultBusy)
		} else {
			s.sendResult(modem.ResultNoCarrier)
		}
		return
	}

	s.remoteMu.Lock()
	s.remoteConn = res.conn
	s.remoteMu.Unlock()

	s.modemHandshakePause()
	s.modem.SetState(modem.StateData)
	s.logger.ConnectionAttempt(number, "success")
	s.sendConnect(entry.RequiredSettings.Baud)
}

// promptPassword prompts the client for a password, shadow-echoing '*' per
// character. Returns true if the entered password matches want. On mismatch
// (or read error) the terminal receives NO CARRIER and the function returns
// false — the dial is aborted.
func (s *Session) promptPassword(number, want string) bool {
	cr := s.modem.Registers().GetCR()
	lf := s.modem.Registers().GetLF()

	if _, err := s.writeClient([]byte(fmt.Sprintf("%c%cPASSWORD: ", cr, lf))); err != nil {
		return false
	}

	if s.reader == nil {
		// Should not happen in normal operation (commandLoop always sets it),
		// but guard for tests that call handleDial directly.
		s.sendResult(modem.ResultNoCarrier)
		return false
	}

	got, err := s.readPassword(s.reader)
	if err != nil {
		s.sendResult(modem.ResultNoCarrier)
		return false
	}

	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		s.logger.ConnectionAttempt(number, "auth_failed")
		s.sendResult(modem.ResultNoCarrier)
		return false
	}
	return true
}

// checkRequiredSettings validates modem settings against phonebook entry requirements.
// Returns true if all settings match (or entry has no requirements).
func (s *Session) checkRequiredSettings(number string, entry *config.PhonebookEntry) bool {
	rs := entry.RequiredSettings
	ok := true

	if rs.Init != "" && !s.modem.HasSentInit(rs.Init) {
		s.logger.MissingSettings(number, "init", rs.Init)
		ok = false
	}

	if rs.Baud != 0 {
		modemBaud := s.modem.GetBaud()
		if modemBaud != rs.Baud {
			s.logger.MissingSettings(number, "baud", fmt.Sprintf("%d (modem: %d)", rs.Baud, modemBaud))
			ok = false
		}
	}

	if rs.ErrorCorrection != nil {
		wantEC := *rs.ErrorCorrection
		haveEC := s.modem.GetErrorCorrection() == 5
		if wantEC != haveEC {
			s.logger.MissingSettings(number, "error_correction", fmt.Sprintf("want %v, have %v", wantEC, haveEC))
			ok = false
		}
	}

	if rs.Compression != nil {
		wantComp := *rs.Compression
		haveComp := s.modem.GetCompression()
		if wantComp != haveComp {
			s.logger.MissingSettings(number, "compression", fmt.Sprintf("want %v, have %v", wantComp, haveComp))
			ok = false
		}
	}

	return ok
}

// modemHandshakePause simulates the carrier detect / protocol negotiation
// delay that real modems exhibit after the remote picks up and before
// reporting CONNECT.  Uses S9 (carrier detect response time) as a base
// plus a random negotiation delay, totaling roughly 2.5-5.5 seconds.
func (s *Session) modemHandshakePause() {
	s9ms := s.modem.Registers().GetCarrierDetectTime() // default 600ms
	time.Sleep(time.Duration(s9ms)*time.Millisecond + time.Duration(2000+rand.Intn(3000))*time.Millisecond)
}

// connectSpeeds is a weighted pool of realistic negotiated line speeds.
// Most connections land at 56000 (V.90), with occasional lower speeds to
// simulate line-quality variation — just like the real thing.
var connectSpeeds = []int{
	56000, 56000, 56000, 56000, 56000, // 50% chance
	33600, 33600, // 20%
	31200, // 10%
	28800, // 10%
	24000, // 10%
}

func pickConnectSpeed() int {
	return connectSpeeds[rand.Intn(len(connectSpeeds))]
}

// sendConnect sends a CONNECT result with a realistic negotiated speed.
// If fixedSpeed > 0, that speed is used instead of a random pick.
func (s *Session) sendConnect(fixedSpeed int) {
	speed := fixedSpeed
	if speed <= 0 {
		modemBaud := s.modem.GetBaud()
		if modemBaud > 0 {
			speed = modemBaud
		} else {
			speed = pickConnectSpeed()
		}
	}
	s.writeClient([]byte(s.modem.FormatConnectResult(speed)))
}

// sendRings sends 1-3 RING results spaced ~3 seconds apart, simulating
// the remote phone ringing before a connection is established.
func (s *Session) sendRings() {
	rings := 1 + rand.Intn(3)
	for i := 0; i < rings; i++ {
		s.sendResult(modem.ResultRing)
		time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
	}
}

// sendNoAnswer simulates dialing a number that rings but nobody picks up,
// just like a real modem would before reporting NO ANSWER to the terminal.
func (s *Session) sendNoAnswer() {
	// Send 4-8 RING results spaced ~3 seconds apart, then NO ANSWER
	rings := 4 + rand.Intn(5)
	for i := 0; i < rings; i++ {
		s.sendResult(modem.ResultRing)
		time.Sleep(time.Duration(2500+rand.Intn(1000)) * time.Millisecond)
	}
	s.sendResult(modem.ResultNoAnswer)
}

// sendIntercept simulates a telco intercept announcement for an invalid number.
// A brief pause (like the "your call cannot be completed" recording) then NO CARRIER,
// with no ringing — so the client can distinguish this from a normal failed connection.
func (s *Session) sendIntercept() {
	time.Sleep(time.Duration(2000+rand.Intn(1500)) * time.Millisecond)
	s.sendResult(modem.ResultNoCarrier)
}

// isConnectionRefused checks if an error is a TCP connection refused error,
// indicating the remote host is up but the port is closed.
func isConnectionRefused(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var sysErr *os.SyscallError
		if errors.As(opErr.Err, &sysErr) {
			return sysErr.Syscall == "connect" && errors.Is(sysErr.Err, syscall.ECONNREFUSED)
		}
	}
	return false
}

// sendGarbageConnect simulates a failed modem negotiation by sending CONNECT
// followed by random garbage characters (as a real modem would produce when
// connecting with incompatible settings), then NO CARRIER.
func (s *Session) sendGarbageConnect() {
	cr := s.modem.Registers().GetCR()
	lf := s.modem.Registers().GetLF()

	// Send CONNECT with speed as if the modem established a link
	speed := pickConnectSpeed()
	s.writeClient([]byte(s.modem.FormatConnectResult(speed)))

	// Send 2-5 bursts of random garbage with small delays between them
	bursts := 2 + rand.Intn(4)
	for i := 0; i < bursts; i++ {
		// Each burst is 8-32 random bytes from crypto/rand
		burstLen := 8 + rand.Intn(25)
		buf := make([]byte, burstLen)
		cryptoRand.Read(buf)
		s.writeClient(buf)
		time.Sleep(time.Duration(100+rand.Intn(300)) * time.Millisecond)
	}

	// Terminate with CR/LF and NO CARRIER
	s.writeClient([]byte{cr, lf})
	s.sendResult(modem.ResultNoCarrier)
}
