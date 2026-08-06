package main

import "testing"

// TestResolveCommand protects the merged binary's dispatch entry point
// (bench/agent/CLI subcommands vs. the server-start flag.Parse() path) so a
// future reordering around flag.Parse() can't silently break routing.
func TestResolveCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no args starts server", []string{}, "server"},
		{"bench subcommand", []string{"bench"}, "bench"},
		{"agent subcommand", []string{"agent"}, "agent"},
		{"cli version subcommand", []string{"version"}, "cli"},
		{"cli status subcommand", []string{"status"}, "cli"},
		{"cli login subcommand", []string{"login"}, "cli"},
		{"cli logout subcommand", []string{"logout"}, "cli"},
		{"cli whoami subcommand", []string{"whoami"}, "cli"},
		{"cli nodes subcommand", []string{"nodes"}, "cli"},
		{"cli models subcommand", []string{"models"}, "cli"},
		{"cli runtime subcommand", []string{"runtime", "restart", "gpu-0"}, "cli"},
		{"cli node control subcommand", []string{"node", "control", "probe"}, "cli"},
		{"top-level help word", []string{"help"}, "help"},
		{"top-level -h flag", []string{"-h"}, "help"},
		{"top-level --help flag", []string{"--help"}, "help"},
		{"root -version flag falls through to server", []string{"-version"}, "server"},
		{"root -db flag falls through to server", []string{"-db", "mesh.db"}, "server"},
		{"root -seed-node flag falls through to server", []string{"-seed-node", "name=a,url=http://x"}, "server"},
		{"unknown token falls through to server", []string{"bogus"}, "server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveCommand(tt.args); got != tt.want {
				t.Errorf("resolveCommand(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
