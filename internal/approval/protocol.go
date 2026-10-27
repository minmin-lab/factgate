package approval

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"taskbound.local/agent-data-gateway/internal/domain"
)

const (
	manifestDigestDomain   = "TASKGATE-MANIFEST-V1\x00"
	grantCoreDigestDomain  = "TASKGATE-GRANT-CORE-V1\x00"
	receiptSignatureDomain = "TASKGATE-APPROVAL-RECEIPT-V1\x00"
	demoReceiptKeyID       = "oa-demo-ed25519-v1"
)

var (
	ErrCanonicalJSON       = errors.New("cannot produce RFC 8785 canonical JSON")
	ErrInvalidReceiptKey   = errors.New("invalid approval receipt key")
	ErrUnknownReceiptKey   = errors.New("unknown approval receipt key ID")
	ErrReceiptKeyNotValid  = errors.New("approval receipt key not valid at issuance time")
	ErrInvalidReceiptSig   = errors.New("invalid approval receipt signature")
	ErrProtocolDigestMatch = errors.New("protocol digest does not match")
)

type AuthorizationManifestV1 = domain.AuthorizationManifestV1
type AuthorizationBudgetV1 = domain.AuthorizationBudgetV1
type TaskGrantCoreV1 = domain.TaskGrantCoreV1
type TaskGrantV1 = domain.TaskGrantV1
type ApprovalReceiptV1 = domain.ApprovalReceiptV1
type ApprovalDecision = domain.ApprovalDecision

const (
	ApprovalDecisionApprove = domain.ApprovalDecisionApprove
	ApprovalDecisionReject  = domain.ApprovalDecisionReject
	ApprovalDecisionNarrow  = domain.ApprovalDecisionNarrow
)

// DraftRequest keeps OA routing metadata outside the immutable manifest. The
// approver route controls who may decide; it does not change what is granted.
type DraftRequest struct {
	Manifest       AuthorizationManifestV1 `json:"authorization_manifest"`
	ManifestDigest string                  `json:"manifest_digest"`
	ApprovalMode   string                  `json:"approval_mode"`
	Approver       string                  `json:"approver,omitempty"`
	// IssuedBy is the subject who issued the task for its human subject and
	// may submit the draft in their place (KB1-LOOP-147); empty otherwise.
	IssuedBy string `json:"issued_by,omitempty"`
	// MemberConfirm marks a child task delegated to another member's agent
	// (KB1-LOOP-148): Approver is that member, who may also submit the draft;
	// the manifest's human subject stays the parent's. False otherwise.
	MemberConfirm bool `json:"member_confirm,omitempty"`
	// ParentExpiresAt is the parent grant's expiry of a delegated task. A
	// grant is re-issued at decision time with the manifest's TTL, so an
	// approver must shorten the TTL to what the parent has left when the
	// decision comes later than the request; the OA does so and decides
	// "narrowed" (KB1-LOOP-147). Nil for a root task.
	ParentExpiresAt *time.Time `json:"parent_expires_at,omitempty"`
	// Submitted marks a draft the agent already submitted for its human
	// subject (KB1-LOOP-149; the Catalog's delegated_authorization
	// agent_submits): the task is AWAITING_APPROVAL from its creation, so
	// the OA creates the draft pending, offers no submission and sends no
	// submitted callback. False otherwise.
	Submitted bool `json:"submitted,omitempty"`
}

// CanonicalJSON implements the JSON Canonicalization Scheme in RFC 8785 for
// I-JSON values. Object properties are ordered by UTF-16 code units, strings
// use the required minimal escapes, and numbers use ECMAScript formatting.
func CanonicalJSON(value any) ([]byte, error) {
	if err := validateUTF8Strings(reflect.ValueOf(value), make(map[visit]struct{})); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal: %v", ErrCanonicalJSON, err)
	}
	if err := rejectDuplicateObjectNames(encoded); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var material any
	if err := decoder.Decode(&material); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrCanonicalJSON, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing JSON value", ErrCanonicalJSON)
	}
	result := make([]byte, 0, len(encoded))
	result, err = appendCanonicalJSON(result, material)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func rejectDuplicateObjectNames(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("%w: %v", ErrCanonicalJSON, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON value", ErrCanonicalJSON)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object property name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object property %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("malformed JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("malformed JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

type visit struct {
	typeOf  reflect.Type
	pointer uintptr
}

func validateUTF8Strings(value reflect.Value, seen map[visit]struct{}) error {
	if !value.IsValid() {
		return nil
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return nil
		}
		return validateUTF8Strings(value.Elem(), seen)
	case reflect.Pointer:
		if value.IsNil() {
			return nil
		}
		entry := visit{typeOf: value.Type(), pointer: value.Pointer()}
		if _, visited := seen[entry]; visited {
			return nil
		}
		seen[entry] = struct{}{}
		return validateUTF8Strings(value.Elem(), seen)
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return fmt.Errorf("%w: invalid UTF-8 string", ErrCanonicalJSON)
		}
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		entry := visit{typeOf: value.Type(), pointer: value.Pointer()}
		if _, visited := seen[entry]; visited {
			return nil
		}
		seen[entry] = struct{}{}
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateUTF8Strings(iterator.Key(), seen); err != nil {
				return err
			}
			if err := validateUTF8Strings(iterator.Value(), seen); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if value.IsNil() {
			return nil
		}
		entry := visit{typeOf: value.Type(), pointer: value.Pointer()}
		if _, visited := seen[entry]; visited {
			return nil
		}
		seen[entry] = struct{}{}
		for index := 0; index < value.Len(); index++ {
			if err := validateUTF8Strings(value.Index(index), seen); err != nil {
				return err
			}
		}
	case reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if err := validateUTF8Strings(value.Index(index), seen); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if value.Type().Field(index).PkgPath != "" {
				continue
			}
			if err := validateUTF8Strings(value.Field(index), seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func CanonicalSHA256(value any) (string, error) {
	canonical, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func ManifestDigest(manifest AuthorizationManifestV1) (string, error) {
	if err := manifest.Validate(); err != nil {
		return "", err
	}
	return domainSeparatedDigest(manifestDigestDomain, manifest)
}

func GrantCoreDigest(grant TaskGrantCoreV1) (string, error) {
	if err := grant.Validate(); err != nil {
		return "", err
	}
	return domainSeparatedDigest(grantCoreDigestDomain, grant)
}

func domainSeparatedDigest(domainName string, value any) (string, error) {
	canonical, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(domainName))
	_, _ = hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// AuthorizationSnapshotSHA256 remains as a compatibility name for adapters
// while returning the V1 domain-separated manifest digest.
func AuthorizationSnapshotSHA256(request DraftRequest) (string, error) {
	return ManifestDigest(request.Manifest)
}

func ValidateAuthorizationSnapshot(request DraftRequest) error {
	if err := request.Manifest.Validate(); err != nil {
		return err
	}
	switch request.ApprovalMode {
	case "manual":
		if strings.TrimSpace(request.Approver) == "" {
			return errors.New("manual approval requires an approver")
		}
	default:
		return errors.New("approval_mode must be manual; automatic task approval is disabled")
	}
	provided, err := hex.DecodeString(request.ManifestDigest)
	if err != nil || len(provided) != sha256.Size || request.ManifestDigest != strings.ToLower(request.ManifestDigest) {
		return errors.New("manifest_digest must be a lowercase SHA-256 hex digest")
	}
	expectedHex, err := ManifestDigest(request.Manifest)
	if err != nil {
		return err
	}
	expected, _ := hex.DecodeString(expectedHex)
	if subtle.ConstantTimeCompare(provided, expected) != 1 {
		return ErrProtocolDigestMatch
	}
	return nil
}

type ReceiptSigner interface {
	KeyID() string
	SignReceipt(ApprovalReceiptV1) (ApprovalReceiptV1, error)
}

type ReceiptVerifier interface {
	VerifyReceipt(ApprovalReceiptV1) error
}

type Ed25519ReceiptSigner struct {
	keyID string
	key   ed25519.PrivateKey
}

func NewEd25519ReceiptSigner(keyID string, key ed25519.PrivateKey) (*Ed25519ReceiptSigner, error) {
	if strings.TrimSpace(keyID) == "" || len(key) != ed25519.PrivateKeySize {
		return nil, ErrInvalidReceiptKey
	}
	return &Ed25519ReceiptSigner{keyID: keyID, key: append(ed25519.PrivateKey(nil), key...)}, nil
}

func NewReceiptSignerFromBase64(keyID, encoded string) (ReceiptSigner, error) {
	key, err := decodeBase64Key(encoded)
	if err != nil {
		return nil, ErrInvalidReceiptKey
	}
	if len(key) == ed25519.SeedSize {
		key = ed25519.NewKeyFromSeed(key)
	}
	return NewEd25519ReceiptSigner(keyID, ed25519.PrivateKey(key))
}

func (s *Ed25519ReceiptSigner) KeyID() string { return s.keyID }

func (s *Ed25519ReceiptSigner) SignReceipt(receipt ApprovalReceiptV1) (ApprovalReceiptV1, error) {
	if s == nil || len(s.key) != ed25519.PrivateKeySize || s.keyID == "" {
		return ApprovalReceiptV1{}, ErrInvalidReceiptKey
	}
	if receipt.KeyID != "" && receipt.KeyID != s.keyID {
		return ApprovalReceiptV1{}, ErrInvalidReceiptKey
	}
	receipt.KeyID = s.keyID
	receipt.IssuedAt = receipt.IssuedAt.UTC()
	receipt.Signature = ""
	if err := receipt.ValidateUnsigned(); err != nil {
		return ApprovalReceiptV1{}, err
	}
	payload, err := receiptSigningPayload(receipt)
	if err != nil {
		return ApprovalReceiptV1{}, err
	}
	receipt.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.key, payload))
	return receipt, nil
}

type ReceiptVerifyingKey struct {
	KeyID     string
	PublicKey ed25519.PublicKey
	ValidFrom time.Time
	RetiredAt time.Time
}

type Ed25519ReceiptVerifier struct {
	keys map[string]ReceiptVerifyingKey
}

func NewEd25519ReceiptVerifier(keys map[string]ed25519.PublicKey) (*Ed25519ReceiptVerifier, error) {
	if len(keys) == 0 {
		return nil, ErrInvalidReceiptKey
	}
	keyring := make([]ReceiptVerifyingKey, 0, len(keys))
	for keyID, key := range keys {
		keyring = append(keyring, ReceiptVerifyingKey{KeyID: keyID, PublicKey: key})
	}
	return NewEd25519ReceiptVerifierWithKeyring(keyring)
}

func NewEd25519ReceiptVerifierWithKeyring(keys []ReceiptVerifyingKey) (*Ed25519ReceiptVerifier, error) {
	if len(keys) == 0 {
		return nil, ErrInvalidReceiptKey
	}
	copyKeys := make(map[string]ReceiptVerifyingKey, len(keys))
	for _, key := range keys {
		normalized, err := normalizeReceiptVerifyingKey(key)
		if err != nil {
			return nil, err
		}
		if _, duplicate := copyKeys[normalized.KeyID]; duplicate {
			return nil, ErrInvalidReceiptKey
		}
		copyKeys[normalized.KeyID] = normalized
	}
	return &Ed25519ReceiptVerifier{keys: copyKeys}, nil
}

func NewReceiptVerifierFromBase64(keyID, encoded string) (ReceiptVerifier, error) {
	key, err := decodeBase64Key(encoded)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, ErrInvalidReceiptKey
	}
	return NewEd25519ReceiptVerifier(map[string]ed25519.PublicKey{keyID: ed25519.PublicKey(key)})
}

func decodeBase64Key(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err == nil {
		return key, nil
	}
	return base64.RawURLEncoding.DecodeString(encoded)
}

func (v *Ed25519ReceiptVerifier) VerifyReceipt(receipt ApprovalReceiptV1) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if v == nil {
		return ErrInvalidReceiptKey
	}
	key, ok := v.keys[receipt.KeyID]
	if !ok {
		return ErrUnknownReceiptKey
	}
	if err := key.validFor(receipt.IssuedAt); err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(receipt.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return ErrInvalidReceiptSig
	}
	payload, err := receiptSigningPayload(receipt)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key.PublicKey, payload, signature) {
		return ErrInvalidReceiptSig
	}
	return nil
}

func normalizeReceiptVerifyingKey(key ReceiptVerifyingKey) (ReceiptVerifyingKey, error) {
	key.KeyID = strings.TrimSpace(key.KeyID)
	if key.KeyID == "" || len(key.PublicKey) != ed25519.PublicKeySize {
		return ReceiptVerifyingKey{}, ErrInvalidReceiptKey
	}
	if !key.ValidFrom.IsZero() {
		key.ValidFrom = key.ValidFrom.UTC()
	}
	if !key.RetiredAt.IsZero() {
		key.RetiredAt = key.RetiredAt.UTC()
		if !key.ValidFrom.IsZero() && key.RetiredAt.Before(key.ValidFrom) {
			return ReceiptVerifyingKey{}, ErrInvalidReceiptKey
		}
	}
	key.PublicKey = append(ed25519.PublicKey(nil), key.PublicKey...)
	return key, nil
}

func (key ReceiptVerifyingKey) validFor(issuedAt time.Time) error {
	issuedAt = issuedAt.UTC()
	if !key.ValidFrom.IsZero() && issuedAt.Before(key.ValidFrom) {
		return ErrReceiptKeyNotValid
	}
	if !key.RetiredAt.IsZero() && issuedAt.After(key.RetiredAt) {
		return ErrReceiptKeyNotValid
	}
	return nil
}

type unsignedApprovalReceiptV1 struct {
	Version             string                  `json:"version"`
	ReceiptID           string                  `json:"receipt_id"`
	TaskID              string                  `json:"task_id"`
	Decision            domain.ApprovalDecision `json:"decision"`
	ManifestDigest      string                  `json:"manifest_digest"`
	ApprovedGrantDigest string                  `json:"approved_grant_digest"`
	ApproverID          string                  `json:"approver_id"`
	IssuedAt            time.Time               `json:"issued_at"`
	KeyID               string                  `json:"key_id"`
}

func receiptSigningPayload(receipt ApprovalReceiptV1) ([]byte, error) {
	unsigned := unsignedApprovalReceiptV1{
		Version: receipt.Version, ReceiptID: receipt.ReceiptID, TaskID: receipt.TaskID,
		Decision: receipt.Decision, ManifestDigest: receipt.ManifestDigest,
		ApprovedGrantDigest: receipt.ApprovedGrantDigest, ApproverID: receipt.ApproverID,
		IssuedAt: receipt.IssuedAt, KeyID: receipt.KeyID,
	}
	canonical, err := CanonicalJSON(unsigned)
	if err != nil {
		return nil, err
	}
	return append([]byte(receiptSignatureDomain), canonical...), nil
}

func DemoReceiptSigner(secret []byte) ReceiptSigner {
	seedMaterial := append([]byte("TASKGATE-DEMO-OA-ED25519-V1\x00"), secret...)
	seed := sha256.Sum256(seedMaterial)
	signer, err := NewEd25519ReceiptSigner(demoReceiptKeyID, ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		panic(err)
	}
	return signer
}

func DemoReceiptVerifier(secret []byte) ReceiptVerifier {
	signer := DemoReceiptSigner(secret).(*Ed25519ReceiptSigner)
	publicKey := signer.key.Public().(ed25519.PublicKey)
	verifier, err := NewEd25519ReceiptVerifier(map[string]ed25519.PublicKey{signer.keyID: publicKey})
	if err != nil {
		panic(err)
	}
	return verifier
}

func VerifyTaskGrantV1(verifier ReceiptVerifier, grant TaskGrantV1) error {
	if err := grant.Validate(); err != nil {
		return err
	}
	digest, err := GrantCoreDigest(grant.Core)
	if err != nil {
		return err
	}
	if digest != grant.ApprovalReceipt.ApprovedGrantDigest {
		return ErrProtocolDigestMatch
	}
	return verifier.VerifyReceipt(grant.ApprovalReceipt)
}

func EncodeTaskGrantV1(grant TaskGrantV1) (string, error) {
	if err := grant.Validate(); err != nil {
		return "", err
	}
	digest, err := GrantCoreDigest(grant.Core)
	if err != nil || digest != grant.ApprovalReceipt.ApprovedGrantDigest {
		return "", ErrProtocolDigestMatch
	}
	encoded, err := CanonicalJSON(grant)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func DecodeTaskGrantV1(encoded string) (TaskGrantV1, error) {
	var grant TaskGrantV1
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&grant); err != nil {
		return grant, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return TaskGrantV1{}, errors.New("task grant contains trailing JSON")
	}
	if err := grant.Validate(); err != nil {
		return TaskGrantV1{}, err
	}
	digest, err := GrantCoreDigest(grant.Core)
	if err != nil || digest != grant.ApprovalReceipt.ApprovedGrantDigest {
		return TaskGrantV1{}, ErrProtocolDigestMatch
	}
	return grant, nil
}

func appendCanonicalJSON(dst []byte, value any) ([]byte, error) {
	switch typed := value.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		return strconv.AppendBool(dst, typed), nil
	case string:
		return appendCanonicalString(dst, typed)
	case json.Number:
		formatted, err := canonicalNumber(string(typed))
		if err != nil {
			return nil, err
		}
		return append(dst, formatted...), nil
	case []any:
		dst = append(dst, '[')
		for index, item := range typed {
			if index != 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendCanonicalJSON(dst, item)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		dst = append(dst, '{')
		for index, key := range keys {
			if index != 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendCanonicalString(dst, key)
			if err != nil {
				return nil, err
			}
			dst = append(dst, ':')
			dst, err = appendCanonicalJSON(dst, typed[key])
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	default:
		return nil, fmt.Errorf("%w: unsupported decoded type %T", ErrCanonicalJSON, value)
	}
}

func appendCanonicalString(dst []byte, value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, fmt.Errorf("%w: invalid UTF-8 string", ErrCanonicalJSON)
	}
	const hexDigits = "0123456789abcdef"
	dst = append(dst, '"')
	for _, char := range value {
		switch char {
		case '"', '\\':
			dst = append(dst, '\\', byte(char))
		case '\b':
			dst = append(dst, "\\b"...)
		case '\t':
			dst = append(dst, "\\t"...)
		case '\n':
			dst = append(dst, "\\n"...)
		case '\f':
			dst = append(dst, "\\f"...)
		case '\r':
			dst = append(dst, "\\r"...)
		default:
			if char >= 0 && char <= 0x1f {
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[byte(char)>>4], hexDigits[byte(char)&0x0f])
			} else {
				dst = utf8.AppendRune(dst, char)
			}
		}
	}
	return append(dst, '"'), nil
}

func utf16Less(left, right string) bool {
	leftUnits := utf16.Encode([]rune(left))
	rightUnits := utf16.Encode([]rune(right))
	limit := len(leftUnits)
	if len(rightUnits) < limit {
		limit = len(rightUnits)
	}
	for index := 0; index < limit; index++ {
		if leftUnits[index] != rightUnits[index] {
			return leftUnits[index] < rightUnits[index]
		}
	}
	return len(leftUnits) < len(rightUnits)
}

func canonicalNumber(value string) (string, error) {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return "", fmt.Errorf("%w: invalid I-JSON number %q", ErrCanonicalJSON, value)
	}
	if parsed == 0 {
		return "0", nil
	}
	abs := math.Abs(parsed)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(parsed, 'f', -1, 64), nil
	}
	exponentForm := strconv.FormatFloat(parsed, 'e', -1, 64)
	mantissa, exponent, found := strings.Cut(exponentForm, "e")
	if !found {
		return "", fmt.Errorf("%w: cannot format number %q", ErrCanonicalJSON, value)
	}
	exponentValue, err := strconv.Atoi(exponent)
	if err != nil {
		return "", fmt.Errorf("%w: cannot format exponent %q", ErrCanonicalJSON, exponent)
	}
	sign := ""
	if exponentValue >= 0 {
		sign = "+"
	}
	return mantissa + "e" + sign + strconv.Itoa(exponentValue), nil
}
