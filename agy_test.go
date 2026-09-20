package main

import (
	"reflect"
	"testing"
)

func TestParseAgyModelLines(t *testing.T) {
	models, mapping := parseAgyModelLines(`
gemini-3.6-flash-high	Gemini 3.6 Flash (High)
gemini-3.6-flash-medium    Gemini 3.6 Flash (Medium)
* Claude Sonnet 4.6 (Thinking)
`)
	if len(models) != 3 {
		t.Fatalf("len(models) = %d, want 3: %#v", len(models), models)
	}
	if mapping["agy/gemini-3.6-flash-high"] != "gemini-3.6-flash-high" {
		t.Fatalf("unexpected mapping: %#v", mapping)
	}
	if mapping["agy/claude-sonnet-4-6-thinking"] != "Claude Sonnet 4.6 (Thinking)" {
		t.Fatalf("legacy label mapping missing: %#v", mapping)
	}
}

func TestAgyArgs(t *testing.T) {
	cfg := defaultConfig()
	cfg.DangerouslySkipPermissions = true
	cfg.Sandbox = true
	got := agyArgs(cfg, "gemini-x-high", "stream-json", "hello")
	want := []string{
		"--output-format", "stream-json",
		"--print-timeout", "30m",
		"--dangerously-skip-permissions",
		"--sandbox",
		"--model", "gemini-x-high",
		"-p", "hello",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agyArgs() = %#v, want %#v", got, want)
	}
}

func TestParseAgyJSONOutputUsesTerminalLine(t *testing.T) {
	got, err := parseAgyJSONOutput([]byte("diagnostic\n{\"status\":\"SUCCESS\",\"response\":\"ok\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "SUCCESS" || got.Response != "ok" {
		t.Fatalf("unexpected result: %#v", got)
	}
}
