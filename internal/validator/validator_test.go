package validator_test

import (
	"testing"

	"github.com/vj/docx/internal/validator"
)

func TestValidator(t *testing.T) {
	v := validator.New()
	if !v.Valid() {
		t.Fatal("expected validator to be valid initially")
	}

	v.Check(validator.NotBlank("hello"), "greeting", "must not be blank")
	if !v.Valid() {
		t.Error("expected validator to be valid after passing check")
	}

	v.Check(validator.NotBlank("   "), "blank_field", "must not be blank")
	if v.Valid() {
		t.Error("expected validator to be invalid after failing check")
	}

	if msg, exists := v.Errors["blank_field"]; !exists || msg != "must not be blank" {
		t.Errorf("unexpected error message: %v", v.Errors)
	}

	// Ensure duplicate errors are not overwritten
	v.AddError("blank_field", "different error")
	if v.Errors["blank_field"] != "must not be blank" {
		t.Errorf("expected original error preserved, got %s", v.Errors["blank_field"])
	}

	// Test MaxChars
	if !validator.MaxChars("12345", 5) {
		t.Error("expected MaxChars(12345, 5) to be true")
	}
	if validator.MaxChars("123456", 5) {
		t.Error("expected MaxChars(123456, 5) to be false")
	}
}
