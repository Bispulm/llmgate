// Package chat is the gateway's model of a chat request and its result. It holds
// only what docs/compatibility.md declares, so an adapter can't forward a field
// that was never validated.
package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Role is the author of a message.
type Role string

// Supported roles.
const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one text message.
type Message struct {
	Role    Role
	Content string
}

// Request is a validated chat request.
type Request struct {
	Model    string
	Messages []Message

	// MaxOutputTokens is the client's limit, or 0 if it set none. The API layer
	// replaces 0 with the alias's limit, so an adapter always has a real bound.
	MaxOutputTokens int64
	Temperature     *float64
	TopP            *float64
	Stop            []string

	Stream       bool
	IncludeUsage bool
}

// Validation error codes, as documented in docs/compatibility.md.
const (
	CodeUnsupportedParameter   = "unsupported_parameter"
	CodeUnsupportedRole        = "unsupported_role"
	CodeUnsupportedContentType = "unsupported_content_type"
	CodeInvalidMessageOrder    = "invalid_message_order"
	CodeInvalidValue           = "invalid_value"
	CodeMissingParameter       = "missing_required_parameter"
	CodeDuplicateParameter     = "duplicate_parameter"
	CodeInvalidJSON            = "invalid_json"
)

const (
	maxModelLength     = 256
	maxStopSequences   = 4
	maxTemperature     = 2.0
	maxTopP            = 1.0
	maxEchoedFieldName = 64
)

// ValidationError explains why a request was rejected. Its text names fields but
// never repeats their values, so it is safe to return and to log.
type ValidationError struct {
	Code    string
	Param   string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Param == "" {
		return e.Code + ": " + e.Message
	}
	return e.Code + ": " + e.Param + ": " + e.Message
}

func invalidValue(param, message string) *ValidationError {
	return &ValidationError{Code: CodeInvalidValue, Param: param, Message: param + " " + message}
}

func missing(param string) *ValidationError {
	return &ValidationError{Code: CodeMissingParameter, Param: param, Message: param + " is required"}
}

func unsupported(param string) *ValidationError {
	return &ValidationError{
		Code:    CodeUnsupportedParameter,
		Param:   param,
		Message: param + " is not supported by this gateway",
	}
}

func duplicate(param string) *ValidationError {
	return &ValidationError{Code: CodeDuplicateParameter, Param: param, Message: param + " appears more than once"}
}

func invalidJSON(dec *json.Decoder) *ValidationError {
	return &ValidationError{
		Code:    CodeInvalidJSON,
		Message: fmt.Sprintf("the request body is not valid JSON (near byte %d)", dec.InputOffset()),
	}
}

// boundedName limits a client-supplied field name before it is echoed in an error.
func boundedName(name string) string {
	if len(name) <= maxEchoedFieldName {
		return name
	}
	return strings.ToValidUTF8(name[:maxEchoedFieldName], "") + "..."
}

// DecodeRequest parses and validates a request body. Every error is a
// *ValidationError.
//
// It walks JSON tokens rather than unmarshaling into a struct, which rejects
// duplicate keys, matches field names exactly, and stops at the first
// unsupported field without parsing the rest of the body.
func DecodeRequest(body []byte) (*Request, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	// Decoding would otherwise replace invalid bytes with U+FFFD, silently
	// changing the client's text.
	if !utf8.Valid(body) {
		return nil, &ValidationError{Code: CodeInvalidJSON, Message: "the request body is not valid UTF-8"}
	}

	if err := expectObject(dec); err != nil {
		return nil, err
	}
	state := requestState{seen: make(map[string]bool, 8)}
	for dec.More() {
		key, err := readKey(dec)
		if err != nil {
			return nil, err
		}
		if state.seen[key] {
			return nil, duplicate(boundedName(key))
		}
		state.seen[key] = true
		if err := state.decodeField(dec, key); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, invalidJSON(dec)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalidJSON(dec)
	}
	return state.finish()
}

// requestState is a request being decoded, with what's needed for checks that
// span several fields.
type requestState struct {
	req              Request
	seen             map[string]bool
	hasStreamOptions bool
	maxTokens        *int64
	maxCompletion    *int64
}

func (s *requestState) decodeField(dec *json.Decoder, key string) error {
	var err error
	switch key {
	case "model":
		err = s.decodeModel(dec)
	case "messages":
		s.req.Messages, err = decodeMessages(dec)
	case "max_completion_tokens":
		s.maxCompletion, err = decodeLimit(dec, key)
	case "max_tokens":
		s.maxTokens, err = decodeLimit(dec, key)
	case "temperature":
		s.req.Temperature, err = decodeRange(dec, key, maxTemperature)
	case "top_p":
		s.req.TopP, err = decodeRange(dec, key, maxTopP)
	case "stop":
		s.req.Stop, err = decodeStop(dec)
	case "stream":
		var v *bool
		if v, err = decodeValue[*bool](dec, key); err == nil && v != nil {
			s.req.Stream = *v
		}
	case "stream_options":
		err = s.decodeStreamOptions(dec)
	case "n":
		var v *int64
		if v, err = decodeValue[*int64](dec, key); err == nil && v != nil && *v != 1 {
			err = unsupported(key)
		}
	case "presence_penalty", "frequency_penalty":
		var v *float64
		if v, err = decodeValue[*float64](dec, key); err == nil && v != nil && *v != 0 {
			err = unsupported(key)
		}
	case "logprobs":
		var v *bool
		if v, err = decodeValue[*bool](dec, key); err == nil && v != nil && *v {
			err = unsupported(key)
		}
	default:
		err = unsupported(boundedName(key))
	}
	return err
}

func (s *requestState) decodeModel(dec *json.Decoder) error {
	v, err := decodeValue[*string](dec, "model")
	if err != nil {
		return err
	}
	if v == nil || *v == "" {
		return invalidValue("model", "must be a non-empty string")
	}
	if len(*v) > maxModelLength {
		return invalidValue("model", "is too long")
	}
	s.req.Model = *v
	return nil
}

func (s *requestState) decodeStreamOptions(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return invalidJSON(dec)
	}
	if tok == nil {
		return nil
	}
	if tok != json.Delim('{') {
		return invalidValue("stream_options", "must be an object")
	}
	s.hasStreamOptions = true

	seen := make(map[string]bool, 2)
	for dec.More() {
		key, err := readKey(dec)
		if err != nil {
			return err
		}
		param := "stream_options." + boundedName(key)
		if seen[key] {
			return duplicate(param)
		}
		seen[key] = true

		switch key {
		case "include_usage":
			v, err := decodeValue[*bool](dec, param)
			if err != nil {
				return err
			}
			if v != nil {
				s.req.IncludeUsage = *v
			}
		case "include_obfuscation":
			v, err := decodeValue[*bool](dec, param)
			if err != nil {
				return err
			}
			if v != nil && *v {
				return unsupported(param)
			}
		default:
			return unsupported(param)
		}
	}
	if _, err := dec.Token(); err != nil {
		return invalidJSON(dec)
	}
	return nil
}

func (s *requestState) finish() (*Request, error) {
	if !s.seen["model"] {
		return nil, missing("model")
	}
	if !s.seen["messages"] {
		return nil, missing("messages")
	}
	if s.hasStreamOptions && !s.req.Stream {
		return nil, invalidValue("stream_options", "requires stream to be true")
	}
	switch {
	case s.maxTokens != nil && s.maxCompletion != nil:
		return nil, invalidValue("max_tokens", "cannot be combined with max_completion_tokens")
	case s.maxCompletion != nil:
		s.req.MaxOutputTokens = *s.maxCompletion
	case s.maxTokens != nil:
		s.req.MaxOutputTokens = *s.maxTokens
	}
	if err := validateOrder(s.req.Messages); err != nil {
		return nil, err
	}
	return &s.req, nil
}

// validateOrder enforces rules that make a conversation mean the same thing on
// every provider. See docs/compatibility.md.
func validateOrder(messages []Message) error {
	seenConversation := false
	for i, m := range messages {
		switch m.Role {
		case RoleSystem, RoleDeveloper:
			if seenConversation {
				return &ValidationError{
					Code:    CodeInvalidMessageOrder,
					Param:   fmt.Sprintf("messages[%d].role", i),
					Message: "system and developer messages must come before all other messages",
				}
			}
		default:
			seenConversation = true
		}
	}
	last := len(messages) - 1
	if messages[last].Role != RoleUser {
		return &ValidationError{
			Code:    CodeInvalidMessageOrder,
			Param:   fmt.Sprintf("messages[%d].role", last),
			Message: "the last message must have the role user",
		}
	}
	return nil
}

func decodeMessages(dec *json.Decoder) ([]Message, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, invalidJSON(dec)
	}
	if tok != json.Delim('[') {
		return nil, invalidValue("messages", "must be a non-empty array")
	}
	var messages []Message
	for dec.More() {
		m, err := decodeMessage(dec, len(messages))
		if err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	if _, err := dec.Token(); err != nil {
		return nil, invalidJSON(dec)
	}
	if len(messages) == 0 {
		return nil, invalidValue("messages", "must be a non-empty array")
	}
	return messages, nil
}

func decodeMessage(dec *json.Decoder, index int) (Message, error) {
	var m Message
	prefix := fmt.Sprintf("messages[%d]", index)

	tok, err := dec.Token()
	if err != nil {
		return m, invalidJSON(dec)
	}
	if tok != json.Delim('{') {
		return m, invalidValue(prefix, "must be an object")
	}

	seenRole, seenContent := false, false
	for dec.More() {
		key, err := readKey(dec)
		if err != nil {
			return m, err
		}
		switch key {
		case "role":
			if seenRole {
				return m, duplicate(prefix + ".role")
			}
			seenRole = true
			if m.Role, err = decodeRole(dec, prefix+".role"); err != nil {
				return m, err
			}
		case "content":
			if seenContent {
				return m, duplicate(prefix + ".content")
			}
			seenContent = true
			if m.Content, err = decodeContent(dec, prefix+".content"); err != nil {
				return m, err
			}
		default:
			return m, unsupported(prefix + "." + boundedName(key))
		}
	}
	if _, err := dec.Token(); err != nil {
		return m, invalidJSON(dec)
	}
	if !seenRole {
		return m, missing(prefix + ".role")
	}
	if !seenContent {
		return m, missing(prefix + ".content")
	}
	return m, nil
}

func decodeRole(dec *json.Decoder, param string) (Role, error) {
	v, err := decodeValue[*string](dec, param)
	if err != nil {
		return "", err
	}
	if v == nil {
		return "", invalidValue(param, "must be a string")
	}
	switch role := Role(*v); role {
	case RoleSystem, RoleDeveloper, RoleUser, RoleAssistant:
		return role, nil
	case "tool", "function":
		return "", &ValidationError{
			Code:    CodeUnsupportedRole,
			Param:   param,
			Message: "tool and function messages are not supported by this gateway",
		}
	default:
		return "", invalidValue(param, "must be system, developer, user, or assistant")
	}
}

func decodeContent(dec *json.Decoder, param string) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", invalidJSON(dec)
	}
	switch v := tok.(type) {
	case string:
		if v == "" {
			return "", invalidValue(param, "must not be empty")
		}
		return v, nil
	case json.Delim:
		return "", &ValidationError{
			Code:    CodeUnsupportedContentType,
			Param:   param,
			Message: "only string content is supported by this gateway",
		}
	default:
		return "", invalidValue(param, "must be a string")
	}
}

func decodeLimit(dec *json.Decoder, param string) (*int64, error) {
	v, err := decodeValue[*int64](dec, param)
	if err != nil {
		return nil, err
	}
	if v != nil && *v < 1 {
		return nil, invalidValue(param, "must be at least 1")
	}
	return v, nil
}

func decodeRange(dec *json.Decoder, param string, limit float64) (*float64, error) {
	v, err := decodeValue[*float64](dec, param)
	if err != nil {
		return nil, err
	}
	if v != nil && (*v < 0 || *v > limit) {
		return nil, invalidValue(param, fmt.Sprintf("must be between 0 and %g", limit))
	}
	return v, nil
}

func decodeStop(dec *json.Decoder) ([]string, error) {
	const param = "stop"
	tok, err := dec.Token()
	if err != nil {
		return nil, invalidJSON(dec)
	}
	switch v := tok.(type) {
	case nil:
		return nil, nil
	case string:
		if v == "" {
			return nil, invalidValue(param, "must not contain an empty string")
		}
		return []string{v}, nil
	case json.Delim:
		if v != '[' {
			return nil, invalidValue(param, "must be a string or an array of strings")
		}
	default:
		return nil, invalidValue(param, "must be a string or an array of strings")
	}

	var stop []string
	for dec.More() {
		s, err := decodeValue[*string](dec, param)
		if err != nil {
			return nil, err
		}
		if s == nil || *s == "" {
			return nil, invalidValue(param, "must not contain an empty string")
		}
		stop = append(stop, *s)
		if len(stop) > maxStopSequences {
			return nil, invalidValue(param, fmt.Sprintf("can have at most %d sequences", maxStopSequences))
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, invalidJSON(dec)
	}
	if len(stop) == 0 {
		return nil, invalidValue(param, "must have at least one sequence")
	}
	return stop, nil
}

// decodeValue decodes the next value into T. A value of the wrong type is an
// invalid value. Anything else means the JSON itself is broken.
func decodeValue[T any](dec *json.Decoder, param string) (T, error) {
	var v T
	if err := dec.Decode(&v); err != nil {
		if typeErr := (*json.UnmarshalTypeError)(nil); errors.As(err, &typeErr) {
			return v, invalidValue(param, "has the wrong type")
		}
		return v, invalidJSON(dec)
	}
	return v, nil
}

func expectObject(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return &ValidationError{Code: CodeInvalidJSON, Message: "the request body must be a JSON object"}
	}
	return nil
}

func readKey(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", invalidJSON(dec)
	}
	key, ok := tok.(string)
	if !ok {
		return "", invalidJSON(dec)
	}
	return key, nil
}
