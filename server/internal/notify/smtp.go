package notify

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/wneessen/go-mail"
)

// SMTPSender sends via wneessen/go-mail (MIT, pre-approved).
// Never logs Password.
type SMTPSender struct {
	CFG SMTPConfig
}

// Send implements Sender.
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	if s.CFG.Host == "" {
		return fmt.Errorf("smtp host not configured")
	}
	port := s.CFG.Port
	if port == 0 {
		port = 587
	}
	from := s.CFG.From
	if from == "" {
		from = "breakwater@localhost"
	}
	m := mail.NewMsg()
	if err := m.From(from); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	if err := m.To(msg.To...); err != nil {
		return fmt.Errorf("to: %w", err)
	}
	m.Subject(msg.Subject)
	m.SetBodyString(mail.TypeTextPlain, msg.Body)

	opts := []mail.Option{
		mail.WithPort(port),
	}
	mode := strings.ToLower(s.CFG.TLSMode)
	switch mode {
	case "none", "plain":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	case "tls", "ssl":
		opts = append(opts, mail.WithSSL())
	default:
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}
	if s.CFG.Username != "" {
		opts = append(opts, mail.WithSMTPAuth(mail.SMTPAuthPlain),
			mail.WithUsername(s.CFG.Username),
			mail.WithPassword(s.CFG.Password))
	}

	client, err := mail.NewClient(s.CFG.Host, opts...)
	if err != nil {
		// Error strings must not include password.
		return fmt.Errorf("smtp client %s:%s: %w", s.CFG.Host, strconv.Itoa(port), err)
	}
	defer client.Close()
	if err := client.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}
