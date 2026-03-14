package services

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"

	"github.com/edenzou/channel-service/pkg/internal/models"
)

// EmailService 封装 SMTP 邮件发送
type EmailService struct {
	config *models.Config
}

func NewEmailService(config *models.Config) *EmailService {
	return &EmailService{config: config}
}

// SendVerificationCode 发送验证码邮件
func (s *EmailService) SendVerificationCode(to, code, purpose string) error {
	subject, body := buildVerificationEmail(code, purpose)
	return s.send(to, subject, body)
}

func buildVerificationEmail(code, purpose string) (subject, body string) {
	switch purpose {
	case "register":
		subject = "【Channel Service】邮箱注册验证码"
		body = fmt.Sprintf(`您好，

您正在注册 Channel Service 账号，验证码为：

    %s

验证码 10 分钟内有效，请勿泄露给他人。

如非本人操作，请忽略此邮件。`, code)
	case "login":
		subject = "【Channel Service】登录验证码"
		body = fmt.Sprintf(`您好，

您正在使用邮箱验证码登录 Channel Service，验证码为：

    %s

验证码 10 分钟内有效，请勿泄露给他人。

如非本人操作，请忽略此邮件。`, code)
	default:
		subject = "【Channel Service】验证码"
		body = fmt.Sprintf("您的验证码为：%s，10 分钟内有效。", code)
	}
	return
}

func (s *EmailService) send(to, subject, body string) error {
	cfg := s.config
	if cfg.SMTPHost == "" {
		return fmt.Errorf("SMTP not configured")
	}

	from := cfg.SMTPFrom
	if from == "" {
		from = cfg.SMTPUsername
	}

	msg := buildMIMEMessage(from, to, subject, body)
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)

	// 尝试 STARTTLS（587）或 TLS（465）
	if cfg.SMTPPort == 465 {
		return s.sendTLS(addr, from, to, msg)
	}
	return s.sendSTARTTLS(addr, from, to, msg, cfg.SMTPUsername, cfg.SMTPPassword)
}

func (s *EmailService) sendSTARTTLS(addr, from, to string, msg []byte, username, password string) error {
	host, _, _ := net.SplitHostPort(addr)
	auth := smtp.PlainAuth("", username, password, host)

	tlsCfg := &tls.Config{ServerName: host}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err = c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}

	if err = c.Auth(auth); err != nil {
		return err
	}
	if err = c.Mail(from); err != nil {
		return err
	}
	if err = c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	_, err = w.Write(msg)
	if err != nil {
		return err
	}
	return w.Close()
}

func (s *EmailService) sendTLS(addr, from, to string, msg []byte) error {
	host, _, _ := net.SplitHostPort(addr)
	tlsCfg := &tls.Config{ServerName: host}
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		return err
	}

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer c.Close()

	auth := smtp.PlainAuth("", s.config.SMTPUsername, s.config.SMTPPassword, host)
	if err = c.Auth(auth); err != nil {
		return err
	}
	if err = c.Mail(from); err != nil {
		return err
	}
	if err = c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	_, err = w.Write(msg)
	if err != nil {
		return err
	}
	return w.Close()
}

func buildMIMEMessage(from, to, subject, body string) []byte {
	lines := []string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
		"",
		body,
	}
	return []byte(strings.Join(lines, "\r\n"))
}
