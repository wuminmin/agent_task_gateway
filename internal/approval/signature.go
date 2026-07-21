package approval

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidSignature = errors.New("invalid callback signature")
	ErrStaleTimestamp   = errors.New("callback timestamp is outside the allowed window")
)

func Sign(secret []byte, timestamp string, rawBody []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(rawBody)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func Verify(secret []byte, timestamp, signature string, rawBody []byte, now time.Time, tolerance time.Duration) error {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrStaleTimestamp
	}
	eventTime := time.Unix(seconds, 0)
	delta := now.Sub(eventTime)
	if delta < 0 {
		delta = -delta
	}
	if delta > tolerance {
		return ErrStaleTimestamp
	}
	providedHex, ok := strings.CutPrefix(signature, "v1=")
	if !ok {
		return ErrInvalidSignature
	}
	provided, err := hex.DecodeString(providedHex)
	if err != nil {
		return ErrInvalidSignature
	}
	expected := Sign(secret, timestamp, rawBody)
	expectedBytes, _ := hex.DecodeString(strings.TrimPrefix(expected, "v1="))
	if !hmac.Equal(provided, expectedBytes) {
		return ErrInvalidSignature
	}
	return nil
}
