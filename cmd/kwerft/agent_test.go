package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ehilzinger/kwerft/internal/clusters"
)

func TestCheckConsoleURL(t *testing.T) {
	for url, ok := range map[string]bool{
		"https://ops.example.com":      true,
		"https://ops.example.com/":     true,
		"https://ops.example.com:8443": true,
		"http://ops.example.com":       false,
		"ops.example.com":              false,
		"https://ops.example.com/path": false,
		"https://user@ops.example.com": false,
		"https://ops.example.com/?a=b": false,
		"":                             false,
	} {
		if err := checkConsoleURL(url); (err == nil) != ok {
			t.Errorf("checkConsoleURL(%q) = %v, want ok=%v", url, err, ok)
		}
	}
}

func TestReadTokenRereadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	read := readToken(path)
	if _, err := read(); err == nil {
		t.Error("no file: no error")
	}
	first, second := clusters.NewAgentToken("edge"), clusters.NewAgentToken("edge")
	if err := os.WriteFile(path, []byte(first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := read(); err != nil || tok != first {
		t.Errorf("read = %q %v", tok, err)
	}
	// The mounted Secret changes (a rotated token): the next dial uses it.
	if err := os.WriteFile(path, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, _ := read(); tok != second {
		t.Errorf("read after rotation = %q", tok)
	}
	if err := os.WriteFile(path, []byte("kwft_not_an_agent_token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := read(); err == nil {
		t.Error("a non-agent token was accepted")
	}
}
