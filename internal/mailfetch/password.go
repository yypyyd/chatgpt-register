package mailfetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

const gmailIMAPAddress = "imap.gmail.com:993"

func dialGmailIMAP(ctx context.Context) (imapSession, error) {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tlsDialer := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{
		MinVersion: tls.VersionTLS12, ServerName: "imap.gmail.com",
	}}
	conn, err := tlsDialer.DialContext(ctx, "tcp", gmailIMAPAddress)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(20 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}
	session, err := client.New(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return session, nil
}

func (c *Client) openPasswordIMAP(ctx context.Context, acc Account) (imapSession, error) {
	session, err := dialGmailIMAP(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: IMAP connect: %v", ErrAuthTemporary, err)
	}
	if err := session.Login(acc.Email, acc.Password); err != nil {
		session.Logout()
		if isTemporaryIMAPError(err) {
			return nil, fmt.Errorf("%w: IMAP login transport failure", ErrAuthTemporary)
		}
		return nil, fmt.Errorf("%w: IMAP 账密被拒（谷歌企业邮箱需开启 IMAP，密码一般用应用专用密码）", ErrAuthFailed)
	}
	return session, nil
}

func (c *Client) listPasswordIMAP(ctx context.Context, acc Account, limit int) ([]Message, error) {
	session, err := c.openPasswordIMAP(ctx, acc)
	if err != nil {
		return nil, err
	}
	defer session.Logout()

	folders, err := discoverIMAPFolders(session)
	if err != nil {
		return nil, fmt.Errorf("%w: IMAP LIST: %v", ErrAuthTemporary, err)
	}
	if extra := gmailSpamFolder(session); extra != "" {
		found := false
		for _, folder := range folders {
			if strings.EqualFold(folder, extra) {
				found = true
				break
			}
		}
		if !found {
			folders = append(folders, extra)
		}
	}
	var output []Message
	var firstErr error
	for _, folder := range folders {
		messages, folderErr := listIMAPFolder(session, folder, limit)
		if folderErr != nil {
			if firstErr == nil {
				firstErr = folderErr
			}
			continue
		}
		output = append(output, messages...)
	}
	if len(output) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return sortAndLimit(output, limit), nil
}

func (c *Client) getPasswordIMAP(ctx context.Context, acc Account, messageID string) (Message, error) {
	folder, uid, err := decodeIMAPID(messageID)
	if err != nil {
		return Message{}, err
	}
	session, err := c.openPasswordIMAP(ctx, acc)
	if err != nil {
		return Message{}, err
	}
	defer session.Logout()
	if _, err := session.Select(folder, true); err != nil {
		return Message{}, err
	}
	set := new(imap.SeqSet)
	set.AddNum(uid)
	section := &imap.BodySectionName{Peek: true}
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- session.UidFetch(set, []imap.FetchItem{imap.FetchUid, section.FetchItem()}, messages)
	}()
	var raw io.Reader
	for item := range messages {
		if item != nil && item.Uid == uid {
			raw = item.GetBody(section)
			if raw != nil {
				break
			}
		}
	}
	if err := <-done; err != nil {
		return Message{}, err
	}
	if raw == nil {
		return Message{}, errors.New("IMAP message not found")
	}
	message, err := parseMIMEMessage(raw)
	if err != nil {
		return Message{}, err
	}
	message.ID = messageID
	return message, nil
}

func gmailSpamFolder(session imapSession) string {
	mailboxes := make(chan *imap.MailboxInfo, 16)
	done := make(chan error, 1)
	go func() {
		done <- session.List("", "*", mailboxes)
	}()
	known := map[string]string{}
	for mailbox := range mailboxes {
		known[strings.ToLower(mailbox.Name)] = mailbox.Name
	}
	_ = <-done
	for _, candidate := range []string{"[Gmail]/Spam", "[Google Mail]/Spam", "Spam"} {
		if actual, ok := known[strings.ToLower(candidate)]; ok {
			return actual
		}
	}
	return ""
}
