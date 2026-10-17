package codexreg

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// fetchEmailCode only retries a confirmed absence of mail, not mailbox errors.
// The returned flag shares the resend budget with wrong-code recovery.
func (c *protocolClient) fetchEmailCode(ctx context.Context, landing string, since time.Time) (string, bool, error) {
	code, err := c.in.FetchCode(ctx, since)
	if err == nil || !errors.Is(err, ErrEmailCodeNotReceived) {
		return code, false, err
	}
	if ctx.Err() != nil {
		return "", false, ctx.Err()
	}
	c.in.logf("📨 邮箱读取正常但未发现新验证邮件，请求重发一次")
	resentAt, err := c.resendEmailCode(ctx, landing)
	if err != nil {
		return "", true, err
	}
	code, err = c.in.FetchCode(ctx, resentAt)
	if err != nil {
		return "", true, fmt.Errorf("重发后获取验证码失败: %w", err)
	}
	return code, true, nil
}

func (c *protocolClient) resendEmailCode(ctx context.Context, landing string) (time.Time, error) {
	if ctx.Err() != nil {
		return time.Time{}, ctx.Err()
	}
	// Graph/IMAP timestamps may have second precision. Keeping nanoseconds here
	// would discard a valid message delivered during the same second.
	resentAt := time.Now().UTC().Truncate(time.Second)
	response, err := c.postJSON(ctx, "https://auth.openai.com/api/accounts/email-otp/resend", landing, map[string]any{})
	if err != nil {
		return time.Time{}, fmt.Errorf("重发验证码失败: %w", err)
	}
	if response.Status < 200 || response.Status >= 300 {
		return time.Time{}, fmt.Errorf("重发验证码未被接受 (HTTP %d)", response.Status)
	}
	if code, _, _ := parseAuthError(response.Body); code != "" {
		return time.Time{}, fmt.Errorf("重发验证码被拒 (HTTP %d code=%s)", response.Status, code)
	}
	c.in.logf("📨 重发请求已被接受 (HTTP %d)，等待收件确认", response.Status)
	return resentAt, nil
}
