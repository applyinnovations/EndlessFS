// Package providerbudget models provider request count, marginal cost, and
// expected latency without contacting a provider. Provider adapters supply
// reviewed, versioned economics fixtures and tests feed this package the exact
// wire requests observed for one application operation.
package providerbudget

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/url"
	"strings"
	"time"
)

const gibibyte = int64(1 << 30)

type Role string

const (
	RoleState           Role = "state"
	RoleFile            Role = "file"
	RoleFileDataPlane   Role = "file-data-plane"
	RolePreviewState    Role = "preview-state"
	RolePreviewSource   Role = "preview-source"
	RolePreviewArtifact Role = "preview-artifact"
)

func (r Role) valid() bool {
	switch r {
	case RoleState, RoleFile, RoleFileDataPlane, RolePreviewState, RolePreviewSource, RolePreviewArtifact:
		return true
	default:
		return false
	}
}

type RequestKind string

const (
	RequestObjectHead     RequestKind = "object_head"
	RequestObjectVerify   RequestKind = "object_verify"
	RequestObjectGet      RequestKind = "object_get"
	RequestObjectOpen     RequestKind = "object_open"
	RequestObjectList     RequestKind = "object_list"
	RequestObjectPut      RequestKind = "object_put"
	RequestObjectDelete   RequestKind = "object_delete"
	RequestObjectCopy     RequestKind = "object_copy"
	RequestUploadBegin    RequestKind = "upload_begin"
	RequestUploadResume   RequestKind = "upload_resume"
	RequestUploadProgress RequestKind = "upload_progress"
	RequestUploadAbort    RequestKind = "upload_abort"
	RequestDownloadSign   RequestKind = "download_sign"
	RequestDataUpload     RequestKind = "data_upload"
	RequestDataDownload   RequestKind = "data_download"
	RequestUnclassified   RequestKind = "unclassified"
)

type Event struct {
	Role          Role
	Kind          RequestKind
	Operation     string
	Subsystem     string
	ParallelGroup string
	Target        string
	RequestBytes  int64
	ResponseBytes int64
	StatusCode    int
	Failed        bool
}

type pricingFixture struct {
	SchemaVersion int                          `json:"schemaVersion"`
	Provider      string                       `json:"provider"`
	Profile       string                       `json:"profile"`
	Currency      string                       `json:"currency"`
	SourceURL     string                       `json:"sourceURL"`
	EffectiveDate string                       `json:"effectiveDate"`
	Assumptions   []string                     `json:"assumptions"`
	Requests      map[RequestKind]requestPrice `json:"requests"`
}

type requestPrice struct {
	BillingClass               string `json:"billingClass"`
	UnitRequests               int64  `json:"unitRequests"`
	UnitPricePicoUSD           int64  `json:"unitPricePicoUSD"`
	RequestPicoUSDPerGiB       int64  `json:"requestPicoUSDPerGiB,omitempty"`
	ResponsePicoUSDPerGiB      int64  `json:"responsePicoUSDPerGiB,omitempty"`
	MinimumRequestBytesBilled  int64  `json:"minimumRequestBytesBilled,omitempty"`
	MinimumResponseBytesBilled int64  `json:"minimumResponseBytesBilled,omitempty"`
}

type latencyFixture struct {
	SchemaVersion  int                            `json:"schemaVersion"`
	Provider       string                         `json:"provider"`
	Profile        string                         `json:"profile"`
	SourceURL      string                         `json:"sourceURL"`
	ResearchedDate string                         `json:"researchedDate"`
	Methodology    string                         `json:"methodology"`
	Limitations    []string                       `json:"limitations"`
	SupportingURLs []string                       `json:"supportingSourceURLs"`
	Requests       map[RequestKind]requestLatency `json:"requests"`
}

type requestLatency struct {
	P50Micros                   int64 `json:"p50Micros"`
	P95Micros                   int64 `json:"p95Micros"`
	P99Micros                   int64 `json:"p99Micros"`
	RequestMicrosPerGiB         int64 `json:"requestMicrosPerGiB,omitempty"`
	ResponseMicrosPerGiB        int64 `json:"responseMicrosPerGiB,omitempty"`
	MinimumRequestBytesModeled  int64 `json:"minimumRequestBytesModeled,omitempty"`
	MinimumResponseBytesModeled int64 `json:"minimumResponseBytesModeled,omitempty"`
}

type Model struct {
	provider string
	profile  string
	prices   map[RequestKind]requestPrice
	latency  map[RequestKind]requestLatency
}

func (m Model) Provider() string { return m.provider }
func (m Model) Profile() string  { return m.profile }

type Totals struct {
	Requests          int64
	FailedRequests    int64
	RequestBytes      int64
	ResponseBytes     int64
	CostPicoUSD       int64
	P50Micros         int64
	P95Micros         int64
	P99Micros         int64
	CriticalP50Micros int64
	CriticalP95Micros int64
	CriticalP99Micros int64
	ByRole            map[Role]RoleTotals
	ByKind            map[RequestKind]RoleTotals
	BySubsystem       map[string]RoleTotals
}

type RoleTotals struct {
	Requests       int64
	FailedRequests int64
	RequestBytes   int64
	ResponseBytes  int64
	CostPicoUSD    int64
	P50Micros      int64
	P95Micros      int64
	P99Micros      int64
}

func ParseModel(pricingBody, latencyBody []byte) (Model, error) {
	var pricing pricingFixture
	if err := decodeStrict(pricingBody, &pricing); err != nil {
		return Model{}, fmt.Errorf("decode provider pricing fixture: %w", err)
	}
	var latency latencyFixture
	if err := decodeStrict(latencyBody, &latency); err != nil {
		return Model{}, fmt.Errorf("decode provider latency fixture: %w", err)
	}
	if pricing.SchemaVersion != 1 || latency.SchemaVersion != 1 {
		return Model{}, errors.New("provider economics fixture schema is unsupported")
	}
	if pricing.Provider == "" || pricing.Provider != latency.Provider || pricing.Profile == "" || pricing.Profile != latency.Profile {
		return Model{}, errors.New("provider economics fixture identity does not match")
	}
	if pricing.Currency != "USD" {
		return Model{}, errors.New("provider pricing fixture currency must be USD")
	}
	if err := validateSourceURL(pricing.SourceURL); err != nil {
		return Model{}, fmt.Errorf("provider pricing source: %w", err)
	}
	if err := validateSourceURL(latency.SourceURL); err != nil {
		return Model{}, fmt.Errorf("provider latency source: %w", err)
	}
	if _, err := time.Parse(time.DateOnly, pricing.EffectiveDate); err != nil {
		return Model{}, errors.New("provider pricing effective date is invalid")
	}
	if _, err := time.Parse(time.DateOnly, latency.ResearchedDate); err != nil {
		return Model{}, errors.New("provider latency research date is invalid")
	}
	if len(pricing.Assumptions) == 0 || !validNotes(pricing.Assumptions) {
		return Model{}, errors.New("provider pricing assumptions are invalid")
	}
	if strings.TrimSpace(latency.Methodology) == "" || len(latency.Limitations) == 0 || !validNotes(latency.Limitations) || len(latency.SupportingURLs) == 0 {
		return Model{}, errors.New("provider latency methodology is invalid")
	}
	for _, source := range latency.SupportingURLs {
		if err := validateSourceURL(source); err != nil {
			return Model{}, fmt.Errorf("provider latency supporting source: %w", err)
		}
	}
	if len(pricing.Requests) == 0 || len(pricing.Requests) != len(latency.Requests) {
		return Model{}, errors.New("provider economics request coverage does not match")
	}
	for kind, price := range pricing.Requests {
		if kind == "" || kind == RequestUnclassified || price.BillingClass == "" || price.UnitRequests <= 0 || price.UnitPricePicoUSD < 0 || price.RequestPicoUSDPerGiB < 0 || price.ResponsePicoUSDPerGiB < 0 || price.MinimumRequestBytesBilled < 0 || price.MinimumResponseBytesBilled < 0 {
			return Model{}, fmt.Errorf("provider request %q has invalid pricing", kind)
		}
		measurement, ok := latency.Requests[kind]
		if !ok {
			return Model{}, fmt.Errorf("provider request %q has no latency model", kind)
		}
		if measurement.P50Micros < 0 || measurement.P95Micros < measurement.P50Micros || measurement.P99Micros < measurement.P95Micros || measurement.RequestMicrosPerGiB < 0 || measurement.ResponseMicrosPerGiB < 0 || measurement.MinimumRequestBytesModeled < 0 || measurement.MinimumResponseBytesModeled < 0 {
			return Model{}, fmt.Errorf("provider request %q has invalid latency percentiles", kind)
		}
	}
	for kind := range latency.Requests {
		if _, ok := pricing.Requests[kind]; !ok {
			return Model{}, fmt.Errorf("provider request %q has no pricing model", kind)
		}
	}
	return Model{provider: pricing.Provider, profile: pricing.Profile, prices: pricing.Requests, latency: latency.Requests}, nil
}

func (m Model) Estimate(events []Event) (Totals, error) {
	totals := Totals{ByRole: make(map[Role]RoleTotals), ByKind: make(map[RequestKind]RoleTotals), BySubsystem: make(map[string]RoleTotals)}
	criticalGroups := make(map[string]RoleTotals)
	serialSequence := 0
	for _, event := range events {
		if !event.Role.valid() || event.RequestBytes < 0 || event.ResponseBytes < 0 || !validTraceLabel(event.Operation) || !validTraceLabel(event.Subsystem) || !validTraceLabel(event.ParallelGroup) {
			return Totals{}, errors.New("provider request event is invalid")
		}
		price, ok := m.prices[event.Kind]
		if !ok {
			return Totals{}, fmt.Errorf("provider request %q is unclassified", event.Kind)
		}
		latency := m.latency[event.Kind]
		requestBytesForPrice := max(event.RequestBytes, price.MinimumRequestBytesBilled)
		responseBytesForPrice := max(event.ResponseBytes, price.MinimumResponseBytesBilled)
		cost, err := scaledCeiling(1, price.UnitPricePicoUSD, price.UnitRequests)
		requestCost, requestCostErr := scaledCeiling(requestBytesForPrice, price.RequestPicoUSDPerGiB, gibibyte)
		responseCost, responseCostErr := scaledCeiling(responseBytesForPrice, price.ResponsePicoUSDPerGiB, gibibyte)
		if err == nil {
			err = requestCostErr
		}
		if err == nil {
			err = responseCostErr
		}
		if err == nil {
			cost, err = checkedAdd(cost, requestCost)
		}
		if err == nil {
			cost, err = checkedAdd(cost, responseCost)
		}
		if err != nil {
			return Totals{}, fmt.Errorf("provider request %q cost overflows: %w", event.Kind, err)
		}
		requestBytesForLatency := max(event.RequestBytes, latency.MinimumRequestBytesModeled)
		responseBytesForLatency := max(event.ResponseBytes, latency.MinimumResponseBytesModeled)
		byteMicros, err := scaledCeiling(requestBytesForLatency, latency.RequestMicrosPerGiB, gibibyte)
		responseMicros, responseMicrosErr := scaledCeiling(responseBytesForLatency, latency.ResponseMicrosPerGiB, gibibyte)
		if err == nil {
			err = responseMicrosErr
		}
		if err == nil {
			byteMicros, err = checkedAdd(byteMicros, responseMicros)
		}
		if err != nil {
			return Totals{}, fmt.Errorf("provider request %q latency overflows: %w", event.Kind, err)
		}
		p50, p50Err := checkedAdd(latency.P50Micros, byteMicros)
		p95, p95Err := checkedAdd(latency.P95Micros, byteMicros)
		p99, p99Err := checkedAdd(latency.P99Micros, byteMicros)
		if p50Err != nil || p95Err != nil || p99Err != nil {
			return Totals{}, fmt.Errorf("provider request %q latency overflows", event.Kind)
		}
		failed := int64(0)
		if event.Failed {
			failed = 1
		}
		addition := RoleTotals{Requests: 1, FailedRequests: failed, RequestBytes: event.RequestBytes, ResponseBytes: event.ResponseBytes, CostPicoUSD: cost, P50Micros: p50, P95Micros: p95, P99Micros: p99}
		subsystem := event.Subsystem
		if subsystem == "" {
			subsystem = "unattributed"
		}
		if err := addRoleTotals(&totals, event.Role, event.Kind, subsystem, addition); err != nil {
			return Totals{}, err
		}
		group := event.ParallelGroup
		if group == "" {
			serialSequence++
			group = fmt.Sprintf("serial-%d", serialSequence)
		} else if event.Operation != "" {
			group = event.Operation + "\x00" + group
		}
		current := criticalGroups[group]
		current.P50Micros = max(current.P50Micros, p50)
		current.P95Micros = max(current.P95Micros, p95)
		current.P99Micros = max(current.P99Micros, p99)
		criticalGroups[group] = current
	}
	for _, group := range criticalGroups {
		var err error
		if totals.CriticalP50Micros, err = checkedAdd(totals.CriticalP50Micros, group.P50Micros); err != nil {
			return Totals{}, errors.New("provider economics critical-path totals overflow")
		}
		if totals.CriticalP95Micros, err = checkedAdd(totals.CriticalP95Micros, group.P95Micros); err != nil {
			return Totals{}, errors.New("provider economics critical-path totals overflow")
		}
		if totals.CriticalP99Micros, err = checkedAdd(totals.CriticalP99Micros, group.P99Micros); err != nil {
			return Totals{}, errors.New("provider economics critical-path totals overflow")
		}
	}
	return totals, nil
}

func addRoleTotals(totals *Totals, role Role, kind RequestKind, subsystem string, addition RoleTotals) error {
	currentRole := totals.ByRole[role]
	currentKind := totals.ByKind[kind]
	currentSubsystem := totals.BySubsystem[subsystem]
	values := []*int64{
		&totals.Requests, &totals.FailedRequests, &totals.RequestBytes, &totals.ResponseBytes, &totals.CostPicoUSD, &totals.P50Micros, &totals.P95Micros, &totals.P99Micros,
		&currentRole.Requests, &currentRole.FailedRequests, &currentRole.RequestBytes, &currentRole.ResponseBytes, &currentRole.CostPicoUSD, &currentRole.P50Micros, &currentRole.P95Micros, &currentRole.P99Micros,
		&currentKind.Requests, &currentKind.FailedRequests, &currentKind.RequestBytes, &currentKind.ResponseBytes, &currentKind.CostPicoUSD, &currentKind.P50Micros, &currentKind.P95Micros, &currentKind.P99Micros,
		&currentSubsystem.Requests, &currentSubsystem.FailedRequests, &currentSubsystem.RequestBytes, &currentSubsystem.ResponseBytes, &currentSubsystem.CostPicoUSD, &currentSubsystem.P50Micros, &currentSubsystem.P95Micros, &currentSubsystem.P99Micros,
	}
	additions := make([]int64, 0, len(values))
	for range 4 {
		additions = append(additions, addition.Requests, addition.FailedRequests, addition.RequestBytes, addition.ResponseBytes, addition.CostPicoUSD, addition.P50Micros, addition.P95Micros, addition.P99Micros)
	}
	for index := range values {
		next, err := checkedAdd(*values[index], additions[index])
		if err != nil {
			return errors.New("provider economics totals overflow")
		}
		*values[index] = next
	}
	totals.ByRole[role] = currentRole
	totals.ByKind[kind] = currentKind
	totals.BySubsystem[subsystem] = currentSubsystem
	return nil
}

func validTraceLabel(value string) bool {
	return len(value) <= 128 && !strings.ContainsAny(value, "\r\n\x00")
}

func decodeStrict(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("provider economics fixture has trailing content")
		}
		return err
	}
	return nil
}

func validateSourceURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || strings.ContainsAny(value, "\r\n") {
		return errors.New("URL must be an absolute HTTPS URL")
	}
	return nil
}

func validNotes(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > 1024 || strings.ContainsAny(value, "\r\n") {
			return false
		}
	}
	return true
}

func checkedAdd(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, errors.New("integer overflow")
	}
	return left + right, nil
}

func scaledCeiling(value, rate, unit int64) (int64, error) {
	if value < 0 || rate < 0 || unit <= 0 {
		return 0, errors.New("invalid scaled value")
	}
	if value == 0 || rate == 0 {
		return 0, nil
	}
	product := new(big.Int).Mul(big.NewInt(value), big.NewInt(rate))
	product.Add(product, big.NewInt(unit-1))
	product.Div(product, big.NewInt(unit))
	if !product.IsInt64() {
		return 0, errors.New("integer overflow")
	}
	return product.Int64(), nil
}
