package main

import (
	"errors"
	"testing"
)

func TestParseArgsUsage(t *testing.T) {
	cases := [][]string{
		nil,
		{"-l"},       // missing the leading file argument
		{"file.vma"}, // neither -l nor -m/-o
	}
	for _, args := range cases {
		if _, err := parseArgs(args); !errors.Is(err, errUsage) {
			t.Errorf("parseArgs(%v) = %v, want errUsage", args, err)
		}
	}
}

func TestParseArgsList(t *testing.T) {
	cfg, err := parseArgs([]string{"file.vma", "-l"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !cfg.list || cfg.json || cfg.device != "" || cfg.mountDir != "" {
		t.Errorf("parseArgs = %+v, want list-only", cfg)
	}
}

func TestParseArgsListJSON(t *testing.T) {
	cfg, err := parseArgs([]string{"file.vma", "-l", "--json"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !cfg.list || !cfg.json {
		t.Errorf("parseArgs = %+v, want list+json", cfg)
	}
}

func TestParseArgsMount(t *testing.T) {
	cfg, err := parseArgs([]string{"file.vma", "-m", "drive-scsi0", "-o", "/mnt/disk"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if cfg.list || cfg.device != "drive-scsi0" || cfg.mountDir != "/mnt/disk" {
		t.Errorf("parseArgs = %+v, want mount mode", cfg)
	}
}

func TestParseArgsRejectsInvalidCombinations(t *testing.T) {
	cases := [][]string{
		{"file.vma", "-l", "-m", "drive-scsi0", "-o", "/mnt/disk"}, // -l and -m/-o together
		{"file.vma", "-m", "drive-scsi0"},                          // -m without -o
		{"file.vma", "-o", "/mnt/disk"},                            // -o without -m
		{"file.vma", "--json"},                                     // --json without -l
		{"file.vma", "-l", "extra"},                                // trailing positional arg
	}
	for _, args := range cases {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("parseArgs(%v): want an error, got none", args)
		}
	}
}
