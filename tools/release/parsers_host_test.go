package main

import "testing"

func TestValidParserHostResultSupportsLegacyPartialContract(t *testing.T) {
	result := parserHostResult{
		ABI:        2,
		Recognized: true,
		Partial:    true,
		Diagnostics: []struct {
			Message string `json:"message"`
		}{{Message: "host smoke diagnostic"}},
	}
	if !validParserHostResult(result) {
		t.Fatal("legacy host partial result with classified diagnostic was rejected")
	}
}

func TestValidParserHostResultChecksReportedSkipMetadata(t *testing.T) {
	result := parserHostResult{
		ABI:        2,
		Recognized: true,
		Partial:    true,
		Skipped: []struct {
			File     string `json:"file"`
			Reason   string `json:"reason"`
			Category string `json:"category"`
		}{{File: "/workspace/skip.toml", Reason: "no compatible processor", Category: "catalog-format-unavailable"}},
		Diagnostics: []struct {
			Message string `json:"message"`
		}{{Message: "host smoke diagnostic"}},
	}
	if !validParserHostResult(result) {
		t.Fatal("host result with categorized skip metadata was rejected")
	}
	result.Skipped[0].Category = ""
	if validParserHostResult(result) {
		t.Fatal("host result with incomplete reported skip metadata was accepted")
	}
	result.Skipped = result.Skipped[:0]
	if validParserHostResult(result) {
		t.Fatal("host result with an explicitly empty skip list was accepted")
	}
}

func TestValidParserHostResultRequiresDiagnosticAndPartialSafety(t *testing.T) {
	result := parserHostResult{
		ABI:        2,
		Recognized: true,
		Partial:    true,
		Diagnostics: []struct {
			Message string `json:"message"`
		}{{Message: "host smoke diagnostic"}},
	}
	result.Diagnostics = nil
	if validParserHostResult(result) {
		t.Fatal("host result without diagnostic was accepted")
	}
	result.Diagnostics = []struct {
		Message string `json:"message"`
	}{{Message: "host smoke diagnostic"}}
	result.Partial = false
	if validParserHostResult(result) {
		t.Fatal("host result without legacy cache-safety partial status was accepted")
	}
}
