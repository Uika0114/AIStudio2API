package pricing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const MaxFileBytes = 512 * 1024

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
var modelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)

type rateDefinition struct {
	Input  *float64 `json:"input"`
	Cached *float64 `json:"cached_input"`
	Output *float64 `json:"output"`
}

type longDefinition struct {
	rateDefinition
	Above *int64 `json:"input_tokens_above"`
}

type modelDefinition struct {
	rateDefinition
	Long *longDefinition `json:"long_context,omitempty"`
}

type definition struct {
	Schema     int                        `json:"schema_version"`
	Version    string                     `json:"version"`
	Currency   string                     `json:"currency"`
	UnitTokens int64                      `json:"unit_tokens"`
	Models     map[string]modelDefinition `json:"models"`
}

func checkedRates(value rateDefinition) (rates, error) {
	for _, number := range []*float64{value.Input, value.Cached, value.Output} {
		// A generous upper bound also prevents finite-but-overflowing report costs.
		if number == nil || math.IsNaN(*number) || math.IsInf(*number, 0) || *number < 0 || *number > 1e9 {
			return rates{}, errors.New("missing or invalid rate (allowed USD per million: 0..1000000000)")
		}
	}
	return rates{input: *value.Input, cached: *value.Cached, output: *value.Output}, nil
}

func parse(data []byte, source string, at time.Time) (*Snapshot, error) {
	if len(data) == 0 || len(data) > MaxFileBytes {
		return nil, errors.New("price list is empty or exceeds 512 KiB")
	}
	payload := bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	duplicateCheck := json.NewDecoder(bytes.NewReader(payload))
	if err := uniqueValue(duplicateCheck, 0); err != nil {
		return nil, err
	}
	if _, err := duplicateCheck.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("price list must contain exactly one JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var value definition
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid price list JSON: %w", err)
	}
	if value.Schema != 1 || value.Currency != "USD" || value.UnitTokens != 1_000_000 || !versionPattern.MatchString(value.Version) || len(value.Models) == 0 {
		return nil, errors.New("invalid schema/version/currency/unit or empty models")
	}
	models := make(map[string]modelPrice, len(value.Models))
	for name, model := range value.Models {
		if !modelPattern.MatchString(name) {
			return nil, errors.New("invalid base model ID")
		}
		base, err := checkedRates(model.rateDefinition)
		if err != nil {
			return nil, fmt.Errorf("model %s: %w", name, err)
		}
		price := modelPrice{rates: base}
		if model.Long != nil {
			if model.Long.Above == nil || *model.Long.Above <= 0 {
				return nil, errors.New("long_context requires a positive integer input_tokens_above")
			}
			price.longRates, err = checkedRates(model.Long.rateDefinition)
			if err != nil {
				return nil, fmt.Errorf("model %s long_context: %w", name, err)
			}
			price.threshold = *model.Long.Above
		}
		models[name] = price
	}
	hash := sha256.Sum256(data)
	return &Snapshot{version: value.Version, sha256: hex.EncodeToString(hash[:]), source: source, loadedAt: at, models: models, raw: append([]byte(nil), payload...)}, nil
}

// The standard decoder accepts duplicate keys; a maintained whole-table file must not.
func uniqueValue(decoder *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("price list nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid price list JSON: %w", err)
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || name != strings.ToLower(name) || seen[name] {
				return errors.New("duplicate, nonlowercase, or invalid JSON key")
			}
			seen[name] = true
			if err := uniqueValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := uniqueValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

// Status is management-only metadata plus a copy of the effective, credential-free list.
type Status struct {
	Version    string          `json:"version"`
	SHA256     string          `json:"sha256"`
	Source     string          `json:"source"`
	LoadedAt   time.Time       `json:"loaded_at"`
	ModelCount int             `json:"model_count"`
	LastError  string          `json:"last_error,omitempty"`
	PriceList  json.RawMessage `json:"price_list"`
}

// Catalog swaps an entire immutable table; readers never wait for file I/O.
type Catalog struct {
	current   atomic.Pointer[Snapshot]
	mu        sync.Mutex
	lastError string
}

func NewCatalog() *Catalog {
	catalog := &Catalog{}
	catalog.current.Store(DefaultSnapshot())
	return catalog
}

func (catalog *Catalog) Snapshot() *Snapshot {
	if catalog == nil {
		return DefaultSnapshot()
	}
	return catalog.current.Load()
}

func (catalog *Catalog) PricingStatus() Status {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	snapshot := catalog.current.Load()
	return Status{Version: snapshot.version, SHA256: snapshot.sha256, Source: snapshot.source, LoadedAt: snapshot.loadedAt, ModelCount: len(snapshot.models), LastError: catalog.lastError, PriceList: snapshot.JSON()}
}

// Reload never writes the source file. A bad or missing file leaves the active snapshot intact.
func (catalog *Catalog) Reload(path string) (err error) {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	defer func() {
		catalog.lastError = ""
		if err != nil {
			catalog.lastError = err.Error()
		}
	}()
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("read price list: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return errors.New("price list must be a regular file no larger than 512 KiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open price list: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return fmt.Errorf("read price list: %w", err)
	}
	hash := sha256.Sum256(data)
	old := catalog.current.Load()
	if len(data) <= MaxFileBytes && hex.EncodeToString(hash[:]) == old.sha256 && old.source == path {
		return nil
	}
	snapshot, err := parse(data, path, time.Now().UTC())
	if err != nil {
		return err
	}
	catalog.current.Store(snapshot)
	return nil
}

func (catalog *Catalog) stateToken() string {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	snapshot := catalog.current.Load()
	return snapshot.sha256 + "\x00" + snapshot.source + "\x00" + catalog.lastError
}

// Watch is cancelled with the application; callers do the initial synchronous Reload.
// Only meaningful load/error changes are reported, not every unchanged polling tick.
func (catalog *Catalog) Watch(ctx context.Context, path string, interval time.Duration, changed func(Status)) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			before := catalog.stateToken()
			_ = catalog.Reload(path)
			if changed != nil && before != catalog.stateToken() {
				changed(catalog.PricingStatus())
			}
		}
	}
}
