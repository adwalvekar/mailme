package gws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"mailme/internal/gmail"
)

// The initial probe lets gws populate/refresh its cache before reads fan out.
// Recovery drains readers and elects one probe per failed generation. It never
// reads token storage or claims to repair revoked authorization.
func (c *Client) ensureAuth(ctx context.Context) (uint64, error) {
	if err := c.authMu.lock(ctx, false); err != nil {
		return 0, err
	}
	ready, epoch := c.authReady, c.authEpoch
	c.authMu.unlock(false)
	if ready {
		return epoch, nil
	}
	if err := c.authMu.lock(ctx, true); err != nil {
		return 0, err
	}
	defer c.authMu.unlock(true)
	if c.authReady || time.Now().Before(c.authRetry) {
		return c.authEpoch, c.authErr
	}
	return c.probeAuth(ctx)
}

func (c *Client) recoverAuth(ctx context.Context, epoch uint64) (uint64, error) {
	if err := c.authMu.lock(ctx, true); err != nil {
		return 0, err
	}
	defer c.authMu.unlock(true)
	if epoch != c.authEpoch {
		return c.authEpoch, c.authErr
	}
	return c.probeAuth(ctx)
}

func (c *Client) invalidateAuth(epoch uint64) {
	_ = c.authMu.lock(context.Background(), true)
	defer c.authMu.unlock(true)
	if epoch == c.authEpoch {
		c.authReady = false
		c.authRetry = time.Time{}
	}
}

func (c *Client) probeAuth(ctx context.Context) (uint64, error) {
	params, _ := json.Marshal(map[string]string{"userId": c.email})
	var profile gmail.Profile
	var err error
	for attempt := 0; ; attempt++ {
		err = c.pace.Wait(ctx, gmail.Cost("getProfile"))
		if err == nil {
			err = c.invoke(ctx, []string{"gmail", "users", "getProfile", "--params", string(params), "--format", "json"}, &profile)
		}
		if !gmail.Retryable(err) || attempt >= 3 {
			break
		}
		delay := time.Second << attempt
		var apiErr *gmail.APIError
		if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
			delay = apiErr.RetryAfter
		}
		if err = wait(ctx, delay); err != nil {
			break
		}
	}
	err = operationError(err, "getProfile", "")
	if err == nil && (!strings.EqualFold(profile.Email, c.email) || profile.HistoryID == "") {
		err = errors.New("gws account/profile does not match configured email")
	}
	c.authEpoch++
	c.authErr, c.authReady = err, err == nil
	c.authRetry = time.Now().Add(2 * time.Second)
	return c.authEpoch, err
}

var errMalformedResponse = errors.New("gws returned a malformed response")

func operationError(err error, method, id string) error {
	if err == nil {
		return nil
	}
	var apiErr *gmail.APIError
	if errors.As(err, &apiErr) {
		copy := *apiErr
		copy.Method, copy.ID, copy.Backend = method, id, "gws"
		return &copy
	}
	if method == "messages.get" && errors.Is(err, errMalformedResponse) {
		return &gmail.MalformedMessageError{ID: id}
	}
	return fmt.Errorf("gws method=%s id=%s: %w", gmail.SafeIdentifier(method), gmail.SafeIdentifier(id), err)
}
