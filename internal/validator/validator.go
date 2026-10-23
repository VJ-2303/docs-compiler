package validator

import "strings"

// Validator stores validation error messages for specific struct fields.
type Validator struct {
	Errors map[string]string
}

// New constructs an empty Validator ready for assertions.
func New() *Validator {
	return &Validator{Errors: make(map[string]string)}
}

// Valid returns true if no validation errors were recorded.
func (v *Validator) Valid() bool {
	return len(v.Errors) == 0
}

// AddError records an error message for a given field if not already present.
func (v *Validator) AddError(field, message string) {
	if _, exists := v.Errors[field]; !exists {
		v.Errors[field] = message
	}
}

// Check evaluates a boolean condition; if false, it records the error message.
func (v *Validator) Check(ok bool, field, message string) {
	if !ok {
		v.AddError(field, message)
	}
}

// NotBlank verifies that a string contains at least one non-whitespace character.
func NotBlank(value string) bool {
	return strings.TrimSpace(value) != ""
}

// MaxChars verifies that a string does not exceed a maximum character length.
func MaxChars(value string, max int) bool {
	return len([]rune(value)) <= max
}
