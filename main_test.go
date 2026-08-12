package main

import (
	"log/slog"
	"testing"
)

func TestParseAddr(t *testing.T) {
	tests := []struct {
		input    string
		wantHost string
		wantPort string
		wantOk   bool
	}{
		{":5000", "", "5000", true},
		{"0.0.0.0:5000", "0.0.0.0", "5000", true},
		{"192.168.1.1:9090", "192.168.1.1", "9090", true},
		{"localhost:3128", "localhost", "3128", true},
		{"noport", "noport", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		host, port, ok := parseAddr(tt.input)
		if host != tt.wantHost || port != tt.wantPort || ok != tt.wantOk {
			t.Errorf("parseAddr(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.input, host, port, ok, tt.wantHost, tt.wantPort, tt.wantOk)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"DEBUG", slog.LevelDebug},
		{"WARN", slog.LevelWarn},
		{"ERROR", slog.LevelError},
		{"INFO", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"invalid", slog.LevelInfo},
		{"debug", slog.LevelInfo}, // lowercase not matched
	}
	for _, tt := range tests {
		got := parseLogLevel(tt.input)
		if got != tt.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
