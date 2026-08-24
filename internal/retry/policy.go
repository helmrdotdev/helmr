package retry

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const MaxDelayMilliseconds int64 = 86400000

type Jitter string

const (
	JitterNone = Jitter("none")
	JitterFull = Jitter("full")
)

type Manifest struct {
	Enabled     bool     `json:"enabled"`
	MaxAttempts *int64   `json:"maxAttempts,omitempty"`
	Backoff     *Backoff `json:"backoff,omitempty"`
}

type Backoff struct {
	MinMs  int64  `json:"minMs"`
	MaxMs  int64  `json:"maxMs"`
	Factor int64  `json:"factor"`
	Jitter Jitter `json:"jitter"`
}

func Validate(manifest Manifest) error {
	if !manifest.Enabled {
		if manifest.MaxAttempts != nil || manifest.Backoff != nil {
			return errors.New("disabled retry must contain only enabled")
		}
		return nil
	}
	if manifest.MaxAttempts == nil || *manifest.MaxAttempts < 1 || *manifest.MaxAttempts > 10 {
		return errors.New("enabled retry maxAttempts must be in [1,10]")
	}
	if manifest.Backoff == nil {
		return errors.New("enabled retry requires backoff")
	}
	if manifest.Backoff.MinMs < 1 || manifest.Backoff.MinMs > MaxDelayMilliseconds {
		return fmt.Errorf("retry backoff minMs must be in [1,%d]", MaxDelayMilliseconds)
	}
	if manifest.Backoff.MaxMs < 1 || manifest.Backoff.MaxMs > MaxDelayMilliseconds {
		return fmt.Errorf("retry backoff maxMs must be in [1,%d]", MaxDelayMilliseconds)
	}
	if manifest.Backoff.MinMs > manifest.Backoff.MaxMs {
		return errors.New("retry backoff minMs must not exceed maxMs")
	}
	if manifest.Backoff.Factor < 1 || manifest.Backoff.Factor > 100 {
		return errors.New("retry backoff factor must be an integer in [1,100]")
	}
	if manifest.Backoff.Jitter != JitterNone && manifest.Backoff.Jitter != JitterFull {
		return fmt.Errorf("retry backoff jitter %q is unsupported", manifest.Backoff.Jitter)
	}
	return nil
}

func Parse(raw []byte) (Manifest, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return Manifest{}, fmt.Errorf("canonicalize retry manifest: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode retry manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return Manifest{}, errors.New("retry manifest has trailing JSON values")
	} else if !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("decode retry manifest trailer: %w", err)
	}
	if err := Validate(manifest); err != nil {
		return Manifest{}, err
	}
	completeRaw, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, fmt.Errorf("encode retry manifest: %w", err)
	}
	complete, err := jsoncanon.Transform(completeRaw)
	if err != nil {
		return Manifest{}, fmt.Errorf("canonicalize complete retry manifest: %w", err)
	}
	if !bytes.Equal(canonical, complete) {
		return Manifest{}, errors.New("retry manifest does not match the complete canonical v0 shape")
	}
	return manifest, nil
}

func Delay(
	policy Manifest,
	failedAttempt int32,
	sample func(int64) (int64, error),
) (time.Duration, bool, error) {
	if failedAttempt <= 0 {
		return 0, false, errors.New("failed task attempt number must be positive")
	}
	if err := Validate(policy); err != nil {
		return 0, false, err
	}
	if !policy.Enabled {
		return 0, false, nil
	}
	if int64(failedAttempt) >= *policy.MaxAttempts {
		return 0, false, nil
	}
	base := policy.Backoff.MinMs
	for attempt := int32(1); attempt < failedAttempt && base < policy.Backoff.MaxMs; attempt++ {
		if base > policy.Backoff.MaxMs/policy.Backoff.Factor {
			base = policy.Backoff.MaxMs
			break
		}
		base *= policy.Backoff.Factor
		if base > policy.Backoff.MaxMs {
			base = policy.Backoff.MaxMs
		}
	}
	delay := base
	if policy.Backoff.Jitter == JitterFull {
		if sample == nil {
			sample = sampleMilliseconds
		}
		var err error
		delay, err = sample(base)
		if err != nil {
			return 0, false, err
		}
		if delay < 0 || delay > base {
			return 0, false, errors.New("retry jitter sample is outside its inclusive bound")
		}
	}
	return time.Duration(delay) * time.Millisecond, true, nil
}

func sampleMilliseconds(maximum int64) (int64, error) {
	if maximum < 0 {
		return 0, errors.New("retry jitter maximum is negative")
	}
	value, err := rand.Int(rand.Reader, big.NewInt(maximum+1))
	if err != nil {
		return 0, err
	}
	return value.Int64(), nil
}
