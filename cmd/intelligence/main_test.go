package main

import "testing"

func TestVersionPrefersSourceCommit(t *testing.T) {
	if got := version(func(k string) string {
		if k == "SOURCE_COMMIT" {
			return "deadbeef"
		}
		return ""
	}); got != "deadbeef" {
		t.Fatalf("version = %q, want deadbeef", got)
	}
}

func TestVersionFallsBackWithoutSourceCommit(t *testing.T) {
	if got := version(func(string) string { return "" }); got == "" {
		t.Fatal("version is empty without SOURCE_COMMIT")
	}
}

func TestAddrDefaultsToPort8080(t *testing.T) {
	if got := addr(func(string) string { return "" }); got != ":8080" {
		t.Fatalf("addr = %q, want :8080", got)
	}
	if got := addr(func(k string) string {
		if k == "PORT" {
			return "9000"
		}
		return ""
	}); got != ":9000" {
		t.Fatalf("addr = %q, want :9000", got)
	}
}
