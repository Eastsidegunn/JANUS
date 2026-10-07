package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Eastsidegunn/JANUS/core/policy"
)

func TestExampleFiles(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	worldBytes, err := os.ReadFile(filepath.Join(root, "world-config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseWorldConfig(worldBytes); err != nil {
		t.Fatal(err)
	}
	requestBytes, err := os.ReadFile(filepath.Join(root, "run-request.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, parseErr := parseRunRequest(requestBytes); parseErr != nil {
		t.Fatal(parseErr)
	}
	profileBytes, err := os.ReadFile(filepath.Join(root, "policy-profile.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ParseProfile(profileBytes); err != nil {
		t.Fatal(err)
	}
}
