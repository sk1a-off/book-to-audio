package api

import (
	"errors"
	"testing"
)

func TestValidateRewritePreservesPhraseAllowsPronunciationOnlyChanges(
	t *testing.T,
) {
	t.Parallel()

	original := "Елка, ООО «Вектор»: 12% [lbreak] {HH AH0}."
	rewritten := "Ёлка — о-о-о «вектор»: 12 % [lbreak] {HH AH0}!"

	if err := validateRewritePreservesPhrase(original, rewritten); err != nil {
		t.Fatalf("expected pronunciation-only rewrite to pass: %v", err)
	}
}

func TestValidateRewritePreservesPhraseAllowsCombiningStress(t *testing.T) {
	t.Parallel()

	if err := validateRewritePreservesPhrase(
		"Она открыла замок.",
		"Она́ открыла замо\u0301к.",
	); err != nil {
		t.Fatalf("expected combining stress to pass: %v", err)
	}
}

func TestValidateRewritePreservesPhraseRejectsLostOrAddedContent(t *testing.T) {
	t.Parallel()

	for _, rewritten := range []string{
		"В начале было.",
		"В самом начале было слово.",
		"В начале было дело.",
		"В начале было слово 2.",
	} {
		if err := validateRewritePreservesPhrase(
			"В начале было слово.",
			rewritten,
		); !errors.Is(err, errRewriteChangedPhrase) {
			t.Fatalf("rewrite %q must be rejected, got %v", rewritten, err)
		}
	}
}

func TestValidateRewritePreservesPhraseRejectsNumberExpansion(t *testing.T) {
	t.Parallel()

	if err := validateRewritePreservesPhrase(
		"Глава 12.",
		"Глава двенадцать.",
	); !errors.Is(err, errRewriteChangedPhrase) {
		t.Fatalf("expanded number must be rejected, got %v", err)
	}
}

func TestValidateRewritePreservesPhrasePreservesInlineControlsLiterally(
	t *testing.T,
) {
	t.Parallel()

	for _, rewritten := range []string{
		"Да [sigh].",
		"Да [lbreak] [lbreak].",
		"Да [LBREAK].",
		"Да lbreak.",
	} {
		if err := validateRewritePreservesPhrase(
			"Да [lbreak].",
			rewritten,
		); !errors.Is(err, errRewriteChangedPhrase) {
			t.Fatalf("control rewrite %q must be rejected, got %v", rewritten, err)
		}
	}
}
