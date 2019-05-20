package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"io"
	"log"
	"net"
	"net/mail"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

// The Backend implements SMTP server methods.
type Backend struct {
	out_hostport *string
	verbose      *bool
	archiveEmail *mail.Address
}

func (bkd *Backend) logger(args ...interface{}) {
	if *bkd.verbose {
		log.Println(args)
	}
}

func byteDigitToInt(c byte) (int, error) {
	return strconv.Atoi(string(c))
}

// Make an EnhancedCode type out of three bytes
func makeEnhancedCode(c0, c1, c2 byte) smtp.EnhancedCode {
	d0, err0 := byteDigitToInt(c0)
	d1, err1 := byteDigitToInt(c1)
	d2, err2 := byteDigitToInt(c2)
	if err0 == nil && err1 == nil && err2 == nil {
		return smtp.EnhancedCode{
			d0,
			d1,
			d2,
		}
	}
	return smtp.EnhancedCodeNotSet
}

// Check and convert error to SMTPError type, which includes an enhanced code attribute
func errToSmtpErr(e error) *smtp.SMTPError {
	if smtpErr, ok := e.(*smtp.SMTPError); ok {
		return smtpErr
	}
	if tp, ok := e.(*textproto.Error); ok {
		// promote textproto.Error type
		enh := smtp.EnhancedCodeNotSet
		if len(tp.Msg) >= 6 {
			s := tp.Msg[:6]
			if s[1] == '.' && s[3] == '.' && s[5] == ' ' {
				enh = makeEnhancedCode(s[0], s[2], s[4])
				// remove enhanced code from front of string
				tp.Msg = tp.Msg[6:]
			}
		}
		return &smtp.SMTPError{
			Code:         tp.Code,
			EnhancedCode: enh,
			Message:      tp.Msg,
		}
	}
	// default - we just have text, placeholders for the rest
	return &smtp.SMTPError{
		Code:         0,
		EnhancedCode: smtp.EnhancedCodeNotSet,
		Message:      e.Error(),
	}
}

// Login handles a login command with username and password.
func (bkd *Backend) Login(state *smtp.ConnectionState, username, password string) (smtp.Session, error) {
	var s Session
	s.bkd = bkd
	bkd.logger("~> LOGIN from", state.Hostname, state.RemoteAddr)

	c, err := smtp.Dial(*bkd.out_hostport)
	if err != nil {
		bkd.logger("\t<~ LOGIN error", *bkd.out_hostport, err)
		return nil, err
	}
	bkd.logger("\t<~ LOGIN success", *bkd.out_hostport)
	s.upstream = c

	// STARTTLS on upstream host, checking its cert is also valid
	host, _, _ := net.SplitHostPort(*bkd.out_hostport)
	tlsconfig := &tls.Config{
		InsecureSkipVerify: false,
		ServerName:         host,
	}
	if err = c.StartTLS(tlsconfig); err != nil {
		bkd.logger("\t<~ STARTTLS error", err)
		return nil, err
	}
	bkd.logger("\t<~ STARTTLS success")

	// Authenticate towards upstream host. If rejected, then pass error back to client
	auth := sasl.NewPlainClient("", username, password)
	if err := c.Auth(auth); err != nil {
		bkd.logger("\t<~ AUTH error", err)
		return nil, errToSmtpErr(err)
	}
	bkd.logger("\t<~ AUTH success")
	return &s, nil
}

// AnonymousLogin requires clients to authenticate using SMTP AUTH before sending emails
func (bkd *Backend) AnonymousLogin(state *smtp.ConnectionState) (smtp.Session, error) {
	bkd.logger("-> Anonymous LOGIN attempted from", state.Hostname, state.RemoteAddr)
	return nil, smtp.ErrAuthRequired
}

// A Session is returned after successful login. Here we build up information until it's fully formed, then send the mail upstream
type Session struct {
	mailfrom string
	rcptto   []string // Can have more than one recipient
	upstream *smtp.Client
	bkd      *Backend // The backend that created this session
}

func (s *Session) Mail(from string) error {
	s.bkd.logger("~> MAIL FROM", from)
	if err := s.upstream.Mail(from); err != nil {
		s.bkd.logger("\t<~ MAIL FROM error", err)
		return errToSmtpErr(err)
	}
	s.mailfrom = from
	s.bkd.logger("\t<~ MAIL FROM accepted")
	return nil
}

func (s *Session) Rcpt(to string) error {
	s.bkd.logger("~> RCPT TO", to)
	if err := s.upstream.Rcpt(to); err != nil {
		s.bkd.logger("\t<~ RCPT TO error", err)
		return errToSmtpErr(err)
	}
	s.rcptto = append(s.rcptto, to)
	s.bkd.logger("\t<~ RCPT TO accepted")
	return nil
}

func (s *Session) Data(r io.Reader) error {
	s.bkd.logger("~> DATA")
	w, err := s.upstream.Data()
	if err != nil {
		s.bkd.logger("\t<~ DATA error", err)
		return err
	}

	// Build SparkPost header value for archival - see https://developers.sparkpost.com/api/smtp/
	arch := fmt.Sprintf("X-MSYS-API: {\"archive\":[{\"email\":\"%s\",\"name\":\"%s\"}]}\n",
		s.bkd.archiveEmail.Address, s.bkd.archiveEmail.Name)
	_, err = io.WriteString(w, arch)

	_, err = io.Copy(w, r)
	if err != nil {
		s.bkd.logger("\t<~ DATA io.Copy error", err)
		return err
	}
	err = w.Close()
	if err != nil {
		s.bkd.logger("\t<~ DATA Close error", err)
		return errToSmtpErr(err)
	}
	s.bkd.logger("\t<~ DATA accepted")

	return nil
}

// No action required
func (s *Session) Reset() {
}

func (s *Session) Logout() error {
	// Close the upstream connection gracefully, if it's open
	if s.upstream != nil {
		s.bkd.logger("~> QUIT")
		if err := s.upstream.Quit(); err != nil {
			s.bkd.logger("\t<~ QUIT error", err)
			return errToSmtpErr(err)
		}
		s.bkd.logger("\t<~ QUIT success")
		s.upstream = nil
	}
	s.mailfrom = ""
	s.rcptto = nil
	return nil
}

func main() {
	in_hostport := flag.String("in_hostport", "localhost:587", "Port number to serve incoming SMTP requests")
	out_hostport := flag.String("out_hostport", "smtp.sparkpostmail.com:587", "host:port for onward routing of SMTP requests")
	verboseOpt := flag.Bool("verbose", false, "print out lots of messages")
	certfile := flag.String("certfile", "fullchain.pem", "Certificate file for this server")
	privkeyfile := flag.String("privkeyfile", "privkey.pem", "Private key file for this server")
	serverDebug := flag.String("server_debug", "", "File to write server SMTP conversation for debugging")
	archiveEmail := flag.String("archive_email", "", "Email address to archive a blind copy to (SparkPost only)")
	flag.Parse()

	log.Println("Incoming host:port set to", *in_hostport)
	log.Println("Outgoing host:port set to", *out_hostport)

	// Gather TLS credentials from filesystem, use these with the server and also set the EHLO server name
	cer, err := tls.LoadX509KeyPair(*certfile, *privkeyfile)
	if err != nil {
		log.Fatal(err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{cer}}

	leafCert, err := x509.ParseCertificate(cer.Certificate[0])
	if err != nil {
		log.Fatal(err)
	}
	subjectDN := leafCert.Subject.ToRDNSequence().String()
	subject := strings.Split(subjectDN, "=")[1]
	log.Println("Gathered certificate", *certfile, "and key", *privkeyfile)
	log.Println("Incoming server name will advertise as", subject)

	// Check if blind-copy archive is required and if so, check for valid email address
	arch := &mail.Address{"", ""}
	if *archiveEmail != "" {
		arch, err = mail.ParseAddress(*archiveEmail)
		if err != nil {
			log.Fatal("Archive", err)
		}
	}

	// Set up parameters that the backend will use
	be := &Backend{
		out_hostport: out_hostport,
		verbose:      verboseOpt,
		archiveEmail: arch,
	}
	log.Println("Backend logging", *be.verbose)
	log.Println("Archive email copy sent to: ", be.archiveEmail.String())

	s := smtp.NewServer(be)
	s.Addr = *in_hostport
	s.Domain = subject
	s.ReadTimeout = 60 * time.Second
	s.WriteTimeout = 60 * time.Second
	s.AllowInsecureAuth = true
	s.TLSConfig = config
	if *serverDebug != "" {
		dbgFile, err := os.OpenFile(*serverDebug, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatal(err)
		}
		defer dbgFile.Close()
		s.Debug = dbgFile
		log.Println("Server logging SMTP commands and responses to", dbgFile.Name())
	}

	// Add LOGIN auth method as not available by default, and Windows Send-MailMessage client requires it
	s.EnableAuth(sasl.Login, func(conn *smtp.Conn) sasl.Server {
		return sasl.NewLoginServer(func(username, password string) error {
			state := conn.State()
			session, err := be.Login(&state, username, password)
			if err != nil {
				return err
			}
			conn.SetSession(session)
			return nil
		})
	})

	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
