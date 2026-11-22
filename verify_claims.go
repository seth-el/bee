package bee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const VerifyClaimsToolName = "verify_claims"

const (
	MaxVerifyClaimsDraftBytes    = 8 << 10
	MaxVerifyClaimsSourceBytes   = 12 << 10
	MaxVerifyClaimsEvidenceItems = 64
	MaxVerifyClaimsOutputItems   = 64
	MaxVerifyClaimsItemBytes     = 1024
	MaxVerifyClaimsOutputBytes   = 8 << 10
)

type VerifyClaimsRequest struct {
	Draft      string         `json:"draft"`
	SourceText string         `json:"source_text"`
	Evidence   []EvidenceItem `json:"evidence"`
}

// ClaimVerificationFinding is a claim-like contradiction/staleness record. The
// claim and its anchor are both bounded; anchor must be an exact source span.
type ClaimVerificationFinding struct {
	Claim  string `json:"claim"`
	Anchor string `json:"anchor"`
}

type ClaimVerificationResult struct {
	SupportedClaims                []string                   `json:"supported_claims"`
	UnsupportedClaims              []string                   `json:"unsupported_claims"`
	Contradictions                 []ClaimVerificationFinding `json:"contradictions"`
	StaleIdentifiersPathsEndpoints []ClaimVerificationFinding `json:"stale_identifiers_paths_endpoints"`
	RequiredDeterministicChecks    []string                   `json:"required_deterministic_checks"`
	AdvisoryOnly                   bool                       `json:"advisory_only"`
}

type ClaimVerificationService struct {
	specialist *SpecialistService
}

func NewClaimVerificationService(specialists ...*SpecialistService) *ClaimVerificationService {
	var specialist *SpecialistService
	if len(specialists) > 0 {
		specialist = specialists[0]
	}
	return &ClaimVerificationService{specialist: specialist}
}

func (service *ClaimVerificationService) Verify(ctx context.Context, input VerifyClaimsRequest) (ClaimVerificationResult, error) {
	if service == nil || service.specialist == nil {
		return ClaimVerificationResult{}, errors.New("claim verification specialist is unavailable")
	}
	if err := service.specialist.requireConfiguredLocalChild("claim verification"); err != nil {
		return ClaimVerificationResult{}, err
	}
	if strings.TrimSpace(input.Draft) == "" || strings.TrimSpace(input.SourceText) == "" {
		return ClaimVerificationResult{}, errors.New("verification draft and source_text are required")
	}
	if len(input.Draft) > MaxVerifyClaimsDraftBytes || len(input.SourceText) > MaxVerifyClaimsSourceBytes || len(input.Evidence) > MaxVerifyClaimsEvidenceItems {
		return ClaimVerificationResult{}, errors.New("verification input exceeds bounds")
	}
	knownClaims := make(map[string]struct{}, len(input.Evidence))
	for _, item := range input.Evidence {
		if err := item.validate(input.SourceText); err != nil {
			return ClaimVerificationResult{}, fmt.Errorf("verification evidence: %w", err)
		}
		knownClaims[strings.TrimSpace(item.Claim)] = struct{}{}
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return ClaimVerificationResult{}, fmt.Errorf("encode verification input: %w", err)
	}
	system := `You are Bee's local claim-verification specialist. Return ONLY one JSON object with exactly these keys: supported_claims, unsupported_claims, contradictions, stale_identifiers_paths_endpoints, required_deterministic_checks, advisory_only. Every supported or unsupported claim must be copied exactly from evidence, draft, or source_text. Every contradiction and stale finding must be an object with exactly {"claim":"exact source/draft claim","anchor":"exact source_text span"}; both fields must be grounded in the supplied text. Required deterministic checks are bounded advisory follow-ups and may be newly worded, but never claims of execution. Every array must be present and non-null. advisory_only must be true. Never use tools, browse, mutate state, write memory, expose secrets, or grant authority.`
	maxInput := service.specialist.Config().MaxInputBytes
	if maxInput > 0 && len(payload)+len(system) > maxInput {
		return ClaimVerificationResult{}, fmt.Errorf("verification input exceeds specialist request bound %d", maxInput)
	}
	content, err := service.specialist.callWithPriority(ctx, LocalChildPurposeClaimVerification, LocalChildPriorityHigh, system, []Message{{Role: RoleUser, Content: string(payload)}}, service.specialist.Config().EvidenceTimeoutSeconds)
	if err != nil {
		return ClaimVerificationResult{}, err
	}
	result, err := parseClaimVerificationResult(content, input, knownClaims)
	if err != nil {
		return ClaimVerificationResult{}, err
	}
	return result, nil
}

func parseClaimVerificationResult(content string, input VerifyClaimsRequest, knownClaims map[string]struct{}) (ClaimVerificationResult, error) {
	content = strings.TrimSpace(content)
	if content == "" || len(content) > MaxVerifyClaimsOutputBytes {
		return ClaimVerificationResult{}, errors.New("claim verification response is empty or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(content)))
	decoder.DisallowUnknownFields()
	var result ClaimVerificationResult
	if err := decoder.Decode(&result); err != nil {
		return ClaimVerificationResult{}, fmt.Errorf("parse claim verification JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return ClaimVerificationResult{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &fields); err != nil {
		return ClaimVerificationResult{}, err
	}
	required := []string{"supported_claims", "unsupported_claims", "contradictions", "stale_identifiers_paths_endpoints", "required_deterministic_checks", "advisory_only"}
	for _, key := range required {
		raw, ok := fields[key]
		if !ok {
			return ClaimVerificationResult{}, fmt.Errorf("claim verification response missing %q", key)
		}
		if key != "advisory_only" && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return ClaimVerificationResult{}, fmt.Errorf("claim verification response field %q must be a non-null array", key)
		}
	}
	if !result.AdvisoryOnly {
		return ClaimVerificationResult{}, errors.New("claim verification must be advisory_only")
	}
	for _, claim := range append(append([]string{}, result.SupportedClaims...), result.UnsupportedClaims...) {
		claim = strings.TrimSpace(claim)
		if _, ok := knownClaims[claim]; !ok && !strings.Contains(input.Draft, claim) && !strings.Contains(input.SourceText, claim) {
			return ClaimVerificationResult{}, fmt.Errorf("claim verification returned an ungrounded claim %q", claim)
		}
		if err := validateClaimVerificationTextAgainstCorpus(claim, []string{input.SourceText, input.Draft}, true); err != nil {
			return ClaimVerificationResult{}, err
		}
	}
	for _, finding := range append(append([]ClaimVerificationFinding{}, result.Contradictions...), result.StaleIdentifiersPathsEndpoints...) {
		if err := validateClaimVerificationFinding(finding, input); err != nil {
			return ClaimVerificationResult{}, err
		}
	}
	for _, list := range [][]string{result.SupportedClaims, result.UnsupportedClaims, result.RequiredDeterministicChecks} {
		if len(list) > MaxVerifyClaimsOutputItems {
			return ClaimVerificationResult{}, errors.New("claim verification response has too many items")
		}
		for _, value := range list {
			if err := validateClaimVerificationText(value, input.SourceText, false); err != nil {
				return ClaimVerificationResult{}, err
			}
		}
	}
	if len(result.Contradictions) > MaxVerifyClaimsOutputItems || len(result.StaleIdentifiersPathsEndpoints) > MaxVerifyClaimsOutputItems {
		return ClaimVerificationResult{}, errors.New("claim verification response has too many findings")
	}
	return result, nil
}

func validateClaimVerificationFinding(finding ClaimVerificationFinding, input VerifyClaimsRequest) error {
	finding.Claim = strings.TrimSpace(finding.Claim)
	finding.Anchor = strings.TrimSpace(finding.Anchor)
	if finding.Claim == "" || finding.Anchor == "" || len(finding.Claim) > MaxVerifyClaimsItemBytes || len(finding.Anchor) > MaxVerifyClaimsItemBytes {
		return errors.New("claim verification finding is empty or oversized")
	}
	if !strings.Contains(input.SourceText, finding.Anchor) {
		return fmt.Errorf("claim verification finding anchor is not an exact source span: %q", finding.Anchor)
	}
	if !strings.Contains(input.SourceText, finding.Claim) && !strings.Contains(input.Draft, finding.Claim) {
		return fmt.Errorf("claim verification finding is not grounded: %q", finding.Claim)
	}
	return validateClaimVerificationTextAgainstCorpus(finding.Claim, []string{input.SourceText, input.Draft}, true)
}

func validateClaimVerificationText(value, sourceText string, requireGrounding bool) error {
	return validateClaimVerificationTextAgainstCorpus(value, []string{sourceText}, requireGrounding)
}

func validateClaimVerificationTextAgainstCorpus(value string, corpus []string, requireGrounding bool) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > MaxVerifyClaimsItemBytes {
		return errors.New("claim verification item is empty or oversized")
	}
	sourceText := ""
	if len(corpus) > 0 {
		sourceText = corpus[0]
	}
	if err := validateSensitiveEvidenceTokens(value, sourceText, true); err != nil {
		return fmt.Errorf("claim verification: %w", err)
	}
	if requireGrounding {
		grounded := false
		for _, text := range corpus {
			if strings.Contains(text, value) {
				grounded = true
				break
			}
		}
		if !grounded {
			return fmt.Errorf("claim verification item is not grounded: %q", value)
		}
	}
	return nil
}

type VerifyClaimsTool struct{ Service *ClaimVerificationService }

func (tool VerifyClaimsTool) Definition() ToolDefinition {
	return ToolDefinition{Name: VerifyClaimsToolName, Description: "Advisory, tool-free verification of structured claims against exact supplied source anchors. It does not browse, inspect files, mutate state, or grant authority.", Risk: RiskLow, Mutability: ToolMutabilityReadOnly, Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"draft":{"type":"string","maxLength":8192},"source_text":{"type":"string","maxLength":12288},"evidence":{"type":"array","maxItems":64,"items":{"type":"object","additionalProperties":false,"properties":{"claim":{"type":"string","maxLength":1024},"anchor":{"type":"string","maxLength":1024},"category":{"type":"string","enum":["fact","inference","uncertainty"]},"confidence":{"type":"number","minimum":0,"maximum":1}},"required":["claim","anchor","category","confidence"]}}},"required":["draft","source_text","evidence"]}`)}
}

func (tool VerifyClaimsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if tool.Service == nil {
		return "", errors.New("verify_claims service is unavailable")
	}
	var input VerifyClaimsRequest
	if err := parseArgs(args, &input); err != nil {
		return "", fmt.Errorf("verify_claims arguments: %w", err)
	}
	result, err := tool.Service.Verify(ctx, input)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(result)
	return string(data), err
}
