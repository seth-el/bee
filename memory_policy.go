package bee

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// MemoryPolicy is the parent-owned safety policy for durable memory and its
// bounded read projections. Zero values are not interpreted as permissions;
// use DefaultMemoryPolicy for the safe baseline.
type MemoryPolicy struct {
	AllowExplicitWrites  bool              `json:"allow_explicit_writes"`
	AllowSuggestedWrites bool              `json:"allow_suggested_writes"`
	AllowSensitiveMemory bool              `json:"allow_sensitive_memory"`
	Retention            time.Duration     `json:"retention"`
	MaxRecords           int               `json:"max_records"`
	MaxBytes             int               `json:"max_bytes"`
	ChildAccess          MemoryChildAccess `json:"child_access"`
	MaxContextRecords    int               `json:"max_context_records"`
	MaxContextBytes      int               `json:"max_context_bytes"`
	MaxQueryRecords      int               `json:"max_query_records"`
	MaxQueryBytes        int               `json:"max_query_bytes"`
}

const (
	// Durable memories do not expire merely because they are old. Callers that
	// need temporary memory should set MemoryRecord.ExpiresAt explicitly or
	// opt into a finite policy retention window.
	DefaultMemoryRetention         time.Duration = 0
	DefaultMemoryMaxRecords                      = 10_000
	DefaultMemoryMaxBytes                        = 16 << 20
	DefaultMemoryMaxContextRecords               = 32
	DefaultMemoryMaxContextBytes                 = 64 << 10
	DefaultMemoryMaxQueryRecords                 = 100
	DefaultMemoryMaxQueryBytes                   = 256 << 10
)

// DefaultMemoryPolicy permits only explicit, parent-owned writes. Children may
// read bounded, non-sensitive results and may submit proposals, but cannot
// write durable records directly.
func DefaultMemoryPolicy() MemoryPolicy {
	return MemoryPolicy{
		AllowExplicitWrites:  true,
		AllowSuggestedWrites: false,
		AllowSensitiveMemory: false,
		Retention:            DefaultMemoryRetention,
		MaxRecords:           DefaultMemoryMaxRecords,
		MaxBytes:             DefaultMemoryMaxBytes,
		ChildAccess:          MemoryChildReadOnly,
		MaxContextRecords:    DefaultMemoryMaxContextRecords,
		MaxContextBytes:      DefaultMemoryMaxContextBytes,
		MaxQueryRecords:      DefaultMemoryMaxQueryRecords,
		MaxQueryBytes:        DefaultMemoryMaxQueryBytes,
	}
}

// NewMemoryPolicy is an explicit constructor alias for callers that prefer a
// constructor-shaped API while retaining the same safe defaults.
func NewMemoryPolicy() MemoryPolicy { return DefaultMemoryPolicy() }

func (policy MemoryPolicy) Validate() error {
	if policy.Retention < 0 {
		return errors.New("memory retention must not be negative")
	}
	if policy.MaxRecords <= 0 || policy.MaxRecords > MaxMemoryRecords {
		return fmt.Errorf("memory max_records must be between 1 and %d", MaxMemoryRecords)
	}
	if policy.MaxBytes <= 0 || policy.MaxBytes > MaxMemoryBytes {
		return fmt.Errorf("memory max_bytes must be between 1 and %d", MaxMemoryBytes)
	}
	if policy.MaxContextRecords <= 0 || policy.MaxContextRecords > MaxMemoryContextRecords {
		return fmt.Errorf("memory max_context_records must be between 1 and %d", MaxMemoryContextRecords)
	}
	if policy.MaxContextBytes <= 0 || policy.MaxContextBytes > MaxMemoryContextBytes {
		return fmt.Errorf("memory max_context_bytes must be between 1 and %d", MaxMemoryContextBytes)
	}
	if policy.MaxQueryRecords <= 0 || policy.MaxQueryRecords > MaxMemoryQueryRecords {
		return fmt.Errorf("memory max_query_records must be between 1 and %d", MaxMemoryQueryRecords)
	}
	if policy.MaxQueryBytes <= 0 || policy.MaxQueryBytes > MaxMemoryQueryBytes {
		return fmt.Errorf("memory max_query_bytes must be between 1 and %d", MaxMemoryQueryBytes)
	}
	if !policy.ChildAccess.valid() {
		return fmt.Errorf("memory child_access %q is invalid", policy.ChildAccess)
	}
	return nil
}

func (policy MemoryPolicy) allowsSensitivity(sensitivity MemorySensitivity) error {
	if sensitivity == MemorySensitivitySecret {
		return errors.New("secret memory is never permitted")
	}
	if sensitivity == MemorySensitivitySensitive && !policy.AllowSensitiveMemory {
		return errors.New("sensitive memory is disabled by policy")
	}
	return nil
}

func (policy MemoryPolicy) AllowsWrite(actor MemoryActor, mode MemoryWriteMode, sensitivity MemorySensitivity) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if err := actor.Validate(); err != nil {
		return err
	}
	if actor.Kind != MemoryActorParent {
		return errors.New("only the parent actor may write durable memory")
	}
	if err := policy.allowsSensitivity(sensitivity); err != nil {
		return err
	}
	switch mode {
	case MemoryWriteExplicit:
		if !policy.AllowExplicitWrites {
			return errors.New("explicit memory writes are disabled by policy")
		}
	case MemoryWriteSuggested:
		if !policy.AllowSuggestedWrites {
			return errors.New("suggested memory writes are disabled by policy")
		}
	default:
		return fmt.Errorf("memory write mode %q is invalid", mode)
	}
	return nil
}

func (policy MemoryPolicy) ChildMayRead() bool {
	return policy.ChildAccess == MemoryChildReadOnly
}

func (policy MemoryPolicy) ChildMayPropose() bool {
	return policy.ChildAccess == MemoryChildReadOnly || policy.ChildAccess == MemoryChildProposalOnly
}

func (policy MemoryPolicy) ChildMayWrite() bool { return false }

func validateConfidence(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return errors.New("confidence must be a finite number between 0 and 1")
	}
	return nil
}

func validateTimestamp(label string, value time.Time, required bool) error {
	if required && value.IsZero() {
		return fmt.Errorf("%s is required", label)
	}
	if !value.IsZero() && strings.TrimSpace(value.Format(time.RFC3339Nano)) == "" {
		return fmt.Errorf("%s is invalid", label)
	}
	return nil
}
