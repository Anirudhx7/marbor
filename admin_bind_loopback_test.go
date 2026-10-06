package main

import "testing"

func TestAdminBindIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"[::1]:8080":     true,
		"localhost:8080": true,
		"LOCALHOST":      true,
		":8080":          false,
		"0.0.0.0:8080":   false,
		"example.com:80": false,
	}
	for addr, want := range cases {
		if got := adminBindIsLoopback(addr); got != want {
			t.Errorf("adminBindIsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
